package cli

import (
	"context"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

func init() {
	register("profile add", command{
		usage:   "profile add NAME --role ROLE --account ACCOUNT [--relay URL] [--quota-project ID]",
		summary: "Add a relay profile: reach one Keyring Google account through the local relay.",
		locked:  true,
		run:     runProfileAdd,
	})
	register("profile list", command{usage: "profile list", summary: "List relay profiles.", run: runProfileList})
	register("profile remove", command{
		usage:   "profile remove NAME",
		summary: "Remove a relay profile and its local change-feed state.",
		locked:  true,
		run:     runProfileRemove,
	})
}

// defaultRelay is where the Keyring relay listens on agent machines.
const defaultRelay = "http://127.0.0.1:7700"

type profileOut struct {
	Name         string   `json:"name"`
	Relay        string   `json:"relay"`
	Role         string   `json:"role"`
	Account      string   `json:"account"`
	QuotaProject string   `json:"quota_project,omitempty"`
	Services     []string `json:"services"`
}

func profileView(name string, p state.Profile) profileOut {
	return profileOut{Name: name, Relay: p.Relay, Role: p.Role, Account: p.Account, QuotaProject: p.QuotaProject,
		Services: []string{p.DriveService(), p.UploadService(), p.SheetsService()}}
}

func runProfileAdd(_ context.Context, a *App, args []string) (any, error) {
	const usage = "profile add NAME --role ROLE --account ACCOUNT [--relay URL] [--quota-project ID]"
	fs := newFlags("profile add")
	relay := fs.String("relay", defaultRelay, "the local Keyring relay")
	role := fs.String("role", "", "the Keyring role whose token the relay holds for this account")
	account := fs.String("account", "", "the Keyring Google account name (services google-ACCOUNT-drive, -upload, -sheets)")
	quota := fs.String("quota-project", "", "Google Cloud project to bill (sent as X-Goog-User-Project; Keyring may override it)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, usage); err != nil {
		return nil, err
	}
	for _, f := range []struct{ name, value string }{{"role", *role}, {"account", *account}} {
		if err := required(f.name, f.value, usage); err != nil {
			return nil, err
		}
	}
	name := pos[0]
	if name == state.DirectProfile || !state.ValidName(name) {
		return nil, apperr.Newf(apperr.Usage, "invalid profile name %q", name).
			WithHint("Use lower-case letters, digits, '-' and '_'; \"direct\" is reserved for this machine's own login.")
	}
	p, err := state.NewProfile(*relay, *role, *account, *quota)
	if err != nil {
		return nil, apperr.New(apperr.Usage, err.Error())
	}
	profiles, err := a.Dir.LoadProfiles()
	if err != nil {
		return nil, err
	}
	if old, ok := profiles[name]; ok {
		if old.Relay == p.Relay && old.Role == p.Role && old.Account == p.Account && old.QuotaProject == p.QuotaProject {
			return profileView(name, old), nil
		}
		return nil, apperr.Newf(apperr.Usage, "profile %q already exists with different settings", name).
			WithHint("Run `drivekey profile remove " + name + "` first; its change feeds start over.")
	}
	// A profile of this name may have been removed while a poll was running; start clean.
	if err := a.Dir.RemoveProfileState(name); err != nil {
		return nil, err
	}
	if err := a.Dir.EnsureProfileDir(name); err != nil {
		return nil, err
	}
	profiles[name] = p
	if err := a.Dir.SaveProfiles(profiles); err != nil {
		return nil, err
	}
	return profileView(name, p), nil
}

func runProfileList(_ context.Context, a *App, args []string) (any, error) {
	pos, err := parseFlags(newFlags("profile list"), args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, "profile list"); err != nil {
		return nil, err
	}
	profiles, err := a.Dir.LoadProfiles()
	if err != nil {
		return nil, err
	}
	out := []profileOut{}
	for _, n := range profileNames(profiles) {
		out = append(out, profileView(n, profiles[n]))
	}
	return map[string]any{"profiles": out}, nil
}

func runProfileRemove(_ context.Context, a *App, args []string) (any, error) {
	pos, err := parseFlags(newFlags("profile remove"), args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, "profile remove NAME"); err != nil {
		return nil, err
	}
	name := pos[0]
	profiles, err := a.Dir.LoadProfiles()
	if err != nil {
		return nil, err
	}
	if _, ok := profiles[name]; !ok {
		return nil, apperr.Newf(apperr.ProfileNotFound, "no relay profile named %q", name).WithHint("Run `drivekey profile list`.")
	}
	delete(profiles, name)
	if err := a.Dir.SaveProfiles(profiles); err != nil {
		return nil, err
	}
	if err := a.Dir.RemoveProfileState(name); err != nil {
		return nil, err
	}
	return map[string]any{"removed": name, "note": "Only local state was removed. The role's Keyring token and grants are unchanged; revoke them in the Keyring dashboard."}, nil
}
