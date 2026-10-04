package cli

import (
	"context"
	"os"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gapi"
	"github.com/jerryfane/drivekey/internal/login"
	"github.com/jerryfane/drivekey/internal/setup"
)

func init() {
	register("version", command{usage: "version", summary: "Print the version.", run: runVersion})
	register("status", command{usage: "status", summary: "Show login, project and setup state.", run: runStatus})
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
	StateDir      string `json:"state_dir"`
	Gcloud        string `json:"gcloud,omitempty"`
	GcloudError   any    `json:"gcloud_error,omitempty"`
	Account       string `json:"account,omitempty"`
	Project       string `json:"project,omitempty"`
	SetupComplete bool   `json:"setup_complete"`
	LoginPending  bool   `json:"login_pending"`
	Next          string `json:"next,omitempty"`
}

func runStatus(ctx context.Context, a *App, args []string) (any, error) {
	if _, err := parseFlags(newFlags("status"), args); err != nil {
		return nil, err
	}
	cfg, err := a.Dir.LoadConfig()
	if err != nil {
		return nil, err
	}
	out := statusOut{
		Version:       Version,
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
	if err := a.Dir.Purge(); err != nil {
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
