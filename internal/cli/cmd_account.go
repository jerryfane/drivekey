package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gapi"
	"github.com/jerryfane/drivekey/internal/login"
	"github.com/jerryfane/drivekey/internal/setup"
	"github.com/jerryfane/drivekey/internal/state"
)

func init() {
	register("version", command{usage: "version", summary: "Print the version.", run: runVersion})
	register("status", command{usage: "status", summary: "Show the mode, login, project and setup state (relay mode: relay health and Google account).", run: runStatus})
	register("login", command{
		usage:   "login [--code CODE] [--interactive]",
		summary: "Start a login (prints a URL), or finish it with the code Google shows.",
		locked:  true,
		run:     runLogin,
	})
	register("logout", command{usage: "logout", summary: "Revoke the login and delete local state.", run: runLogout})
	register("setup", command{
		usage:   "setup [--project ID]",
		summary: "Create your own Google Cloud project and enable the Drive and Sheets APIs.",
		locked:  true,
		run:     runSetup,
	})
}

func runVersion(_ context.Context, _ *App, args []string) (any, error) {
	if _, err := parseFlags(newFlags("version"), args); err != nil {
		return nil, err
	}
	return map[string]string{"version": Version}, nil
}

type statusOut struct {
	Version       string `json:"version"`
	Mode          string `json:"mode"`
	StateDir      string `json:"state_dir"`
	Gcloud        string `json:"gcloud,omitempty"`
	GcloudError   any    `json:"gcloud_error,omitempty"`
	Account       string `json:"account,omitempty"`
	Project       string `json:"project,omitempty"`
	SetupComplete bool   `json:"setup_complete"`
	LoginPending  bool   `json:"login_pending"`
	Next          string `json:"next,omitempty"`
}

type relayStatusOut struct {
	Version  string     `json:"version"`
	Mode     string     `json:"mode"`
	StateDir string     `json:"state_dir"`
	Profile  profileOut `json:"profile"`
	// Relay is "ok" when the local relay reaches the Keyring host.
	Relay string `json:"relay"`
	// GoogleUser is the Google account Keyring acts as, read back through the relay.
	GoogleUser string        `json:"google_user,omitempty"`
	Error      *apperr.Error `json:"error,omitempty"`
	Local      string        `json:"local_state"`
	Remote     string        `json:"keyring_state"`
	Next       string        `json:"next,omitempty"`
}

// directOnly refuses a command that manages this machine's own Google login when a relay
// profile is selected: that profile's login lives on the Keyring host.
func (a *App) directOnly(cmd, hint string) error {
	name, p, err := a.relayProfile()
	if err != nil || p == nil {
		return err
	}
	return apperr.Newf(apperr.RelayMode, "`drivekey %s` does nothing for relay profile %q: account %q is signed in on the Keyring host, not on this machine", cmd, name, p.Account).
		WithHint(hint)
}

func runStatus(ctx context.Context, a *App, args []string) (any, error) {
	if _, err := parseFlags(newFlags("status"), args); err != nil {
		return nil, err
	}
	name, p, err := a.relayProfile()
	if err != nil && apperr.As(err).Code == apperr.ProfileRequired {
		profiles, err := a.Dir.LoadProfiles()
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": Version, "mode": "unselected", "state_dir": a.Dir.Root,
			"profiles": profileNames(profiles), "next": "drivekey --profile NAME status"}, nil
	} else if err != nil {
		return nil, err
	}
	if p != nil {
		return relayStatus(ctx, a, name, *p), nil
	}
	cfg, err := a.Dir.LoadConfig()
	if err != nil {
		return nil, err
	}
	out := statusOut{
		Version:       Version,
		Mode:          "direct",
		StateDir:      a.Dir.Root,
		Project:       cfg.Project,
		SetupComplete: cfg.SetupComplete,
		LoginPending:  login.Pending(a.Dir),
	}
	r, err := a.gcloud()
	if err != nil {
		out.GcloudError = apperr.As(err)
		out.Next = "install gcloud"
		return out, nil
	}
	out.Gcloud = r.Path
	if _, statErr := os.Stat(a.Dir.GcloudConfig()); statErr == nil {
		acct, err := r.ActiveAccount(ctx)
		if err != nil {
			out.GcloudError = apperr.As(err)
		}
		out.Account = acct
	}
	if out.Account != cfg.Account {
		out.Project, out.SetupComplete = "", false
	}
	switch {
	case out.LoginPending:
		out.Next = "drivekey login --code CODE"
	case out.Account == "":
		out.Next = "drivekey login"
	case !out.SetupComplete:
		out.Next = "drivekey setup"
	}
	return out, nil
}

// relayStatus checks the relay and reads back which Google account the profile reaches.
func relayStatus(ctx context.Context, a *App, name string, p state.Profile) relayStatusOut {
	out := relayStatusOut{
		Version:  Version,
		Mode:     "relay",
		StateDir: a.Dir.Root,
		Profile:  profileView(name, p),
		Local:    "This machine stores only the profile (relay URL, role and account names) and change-feed positions. No Google login, token or gcloud is used.",
		Remote:   "The Google login of account " + p.Account + " lives on the Keyring host; role " + p.Role + "'s token is held by the relay.",
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := relayHealth(hctx, p.Relay); err != nil {
		out.Relay, out.Error = "unreachable", err
		out.Next = "start the Keyring relay, or check that the Keyring host is up"
		return out
	}
	out.Relay = "ok"
	c, err := a.Clients(ctx)
	if err == nil {
		var about *drive.About
		about, err = c.Drive.About.Get().Fields("user(emailAddress)").Context(ctx).Do()
		if err == nil {
			if about.User != nil && about.User.EmailAddress != "" {
				out.GoogleUser = about.User.EmailAddress
				return out
			}
			err = apperr.New(apperr.GoogleAPI, "Google answered through the relay but did not name the signed-in account")
		}
	}
	out.Error = gapi.MapError(err)
	switch out.Error.Code {
	case apperr.RelayDenied:
		out.Next = "the owner approves role " + p.Role + " and grants its services in the Keyring dashboard"
	case apperr.RelayLoginRequired:
		out.Next = "the owner signs in account " + p.Account + " on the Keyring host"
	default:
		out.Next = "retry `drivekey --profile " + name + " status`"
	}
	return out
}

// relayHealth asks the local relay whether it reaches the Keyring host.
func relayHealth(ctx context.Context, relay string) *apperr.Error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relay+"/_relay/health", nil)
	if err != nil {
		return apperr.As(err)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return apperr.Wrap(apperr.RelayUnreachable, "cannot reach the Keyring relay at "+relay, err).
			WithHint("Start the local relay (`systemctl status keyring-relay`). drivekey never falls back to a local login.")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return apperr.Newf(apperr.RelayUnreachable, "Keyring relay at %s: %s", relay, strings.TrimSpace(string(b))).
			WithHint("The relay is running but cannot reach the Keyring host. drivekey never falls back to a local login.")
	}
	return nil
}

func runLogin(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "login [--code CODE] [--interactive]"
	fs := newFlags("login")
	code := fs.String("code", "", "the code Google showed after you logged in")
	interactive := fs.Bool("interactive", false, "run the login in this terminal")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, usage); err != nil {
		return nil, err
	}
	if err := a.directOnly("login", "The owner signs that account in on the Keyring host and approves the role there. Check it with `drivekey --profile NAME status`."); err != nil {
		return nil, err
	}
	if *code != "" {
		account, err := login.Submit(ctx, a.Dir, *code)
		if err != nil {
			return nil, err
		}
		return map[string]string{"account": account, "next": "drivekey setup"}, nil
	}
	r, err := a.gcloud()
	if err != nil {
		return nil, err
	}
	if *interactive {
		account, err := login.Interactive(ctx, a.Dir, r)
		if err != nil {
			return nil, err
		}
		return map[string]string{"account": account, "next": "drivekey setup"}, nil
	}
	url, err := login.Start(ctx, a.Dir, r)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"url":          url,
		"instructions": "Open the URL, log in with your Google account, allow access, then copy the code Google shows.",
		"next":         "drivekey login --code CODE",
	}, nil
}

func runLogout(ctx context.Context, a *App, args []string) (any, error) {
	if _, err := parseFlags(newFlags("logout"), args); err != nil {
		return nil, err
	}
	if err := a.directOnly("logout", "To stop using the profile on this machine run `drivekey profile remove NAME`; the owner revokes the role in the Keyring dashboard."); err != nil {
		return nil, err
	}
	if !a.Dir.Owned() {
		return map[string]any{"logged_out": true, "revoked": false, "note": "no drivekey state at " + a.Dir.Root}, nil
	}
	l, err := a.Dir.Lock(ctx, lockWait)
	if err != nil {
		return nil, err
	}
	cfg, err := a.Dir.LoadConfig()
	if err != nil {
		l.Unlock()
		return nil, err
	}
	// A pending login helper would otherwise outlive its directory and could still finish a login.
	login.Stop(a.Dir)
	revoked := false
	if r, err := a.gcloud(); err == nil {
		if _, err := r.Run(ctx, "auth", "revoke", "--all"); err == nil {
			revoked = true
		}
	}
	err = a.Dir.Clear()
	l.Unlock()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"logged_out": true, "revoked": revoked, "state_deleted": a.Dir.Root}
	if cfg.Project != "" {
		out["project_kept"] = cfg.Project
		out["hint"] = "The Google Cloud project was not deleted. Delete it at https://console.cloud.google.com/cloud-resource-manager if you no longer need it."
	}
	return out, nil
}

func runSetup(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "setup [--project ID]"
	fs := newFlags("setup")
	project := fs.String("project", "", "use this existing or new project id instead of a generated one")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, usage); err != nil {
		return nil, err
	}
	if err := a.directOnly("setup", "The Cloud project and APIs belong to the account on the Keyring host. To bill a project of your choice, add the profile with --quota-project; Keyring may set its own."); err != nil {
		return nil, err
	}
	r, err := a.gcloud()
	if err != nil {
		return nil, err
	}
	if err := a.Dir.Ensure(); err != nil {
		return nil, err
	}
	return setup.Run(ctx, a.Dir, r, setup.Options{
		Project: *project,
		Verify: func(ctx context.Context, project string) error {
			cfg, err := a.Dir.LoadConfig()
			if err != nil {
				return err
			}
			c, err := gapi.New(ctx, gapi.NewTokenSource(r, a.Dir, cfg.Account), project)
			if err != nil {
				return err
			}
			if _, err := c.Drive.About.Get().Fields("user(emailAddress)").Context(ctx).Do(); err != nil {
				return gapi.MapError(err)
			}
			// The Sheets API answers 404 for a missing spreadsheet once it is enabled; anything
			// else (401, 403, 5xx) means Sheets is not usable yet.
			_, err = c.Sheets.Spreadsheets.Get("drivekey-setup-probe").Fields("spreadsheetId").Context(ctx).Do()
			if e := gapi.MapError(err); e == nil || e.Code != apperr.NotFound {
				if e == nil {
					return apperr.New(apperr.GoogleAPI, "Sheets setup probe unexpectedly succeeded")
				}
				return e
			}
			return nil
		},
	})
}
