// Package setup creates the user's own Google Cloud project and enables the APIs drivekey uses.
package setup

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

// APIs are enabled in the user's project.
var APIs = []string{"drive.googleapis.com", "sheets.googleapis.com"}

// Runner is the subset of gcloud.Runner setup needs.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Options controls Setup. Verify checks that the APIs answer for project; it is retried
// while it returns an apperr.APIDisabled error (enabling takes a moment to propagate).
type Options struct {
	Project  string
	Verify   func(ctx context.Context, project string) error
	Sleep    func(time.Duration)
	Attempts int
}

// Result is printed by `drivekey setup`.
type Result struct {
	Account string   `json:"account"`
	Project string   `json:"project"`
	Created bool     `json:"created"`
	APIs    []string `json:"apis"`
}

// Run performs setup. It is idempotent: a saved project is reused.
func Run(ctx context.Context, d state.Dir, r Runner, o Options) (Result, error) {
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Attempts == 0 {
		o.Attempts = 6
	}
	account, err := r.Run(ctx, "auth", "list", "--filter=status:ACTIVE", "--format=value(account)")
	if err != nil {
		return Result{}, err
	}
	if account == "" {
		return Result{}, apperr.New(apperr.NotLoggedIn, "not logged in").WithHint("Run `drivekey login`.")
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		return Result{}, err
	}
	if cfg.Account != account {
		cfg = state.Config{Account: account}
	}
	switch {
	case o.Project != "":
		if o.Project != cfg.Project {
			cfg.Project, cfg.SetupComplete = o.Project, false
		}
	case cfg.Project == "":
		cfg.Project = NewProjectID()
	}
	// Save the id before creating, so an interrupted setup reuses it instead of creating another project.
	if err := d.SaveConfig(cfg); err != nil {
		return Result{}, err
	}
	res := Result{Account: account, Project: cfg.Project, APIs: APIs}

	created, err := ensureProject(ctx, r, cfg.Project, o)
	if err != nil {
		return res, err
	}
	res.Created = created
	enable := append([]string{"services", "enable"}, APIs...)
	enable = append(enable, "--project", cfg.Project)
	if _, err := retry(ctx, o, func() (string, error) { return r.Run(ctx, enable...) }); err != nil {
		return res, err
	}
	if o.Verify != nil {
		var verr error
		for range 12 {
			verr = o.Verify(ctx, cfg.Project)
			if verr == nil || apperr.As(verr).Code != apperr.APIDisabled {
				break
			}
			o.Sleep(10 * time.Second)
		}
		if verr != nil {
			return res, verr
		}
	}
	cfg.SetupComplete = true
	return res, d.SaveConfig(cfg)
}

// ensureProject makes sure project exists and is active; it reports whether it created it.
func ensureProject(ctx context.Context, r Runner, project string, o Options) (bool, error) {
	describe := func() (string, error) {
		return r.Run(ctx, "projects", "describe", project, "--format=value(lifecycleState)")
	}
	for attempt := 0; ; attempt++ {
		st, err := describe()
		if err == nil {
			if st != "ACTIVE" {
				return false, apperr.Newf(apperr.GcloudFailed, "project %s is %s", project, st).
					WithHint("Pass a different project with `drivekey setup --project ID`, or restore it in the Cloud console.")
			}
			return false, nil
		}
		if c := apperr.As(err).Code; c == apperr.NotLoggedIn {
			return false, err
		}
		_, err = r.Run(ctx, "projects", "create", project, "--name=drivekey")
		if err == nil {
			return true, nil
		}
		e := apperr.As(err)
		if e.Code != apperr.RateLimited || attempt+1 >= o.Attempts {
			if e.Code == apperr.GcloudFailed && strings.Contains(e.Message, "already in use") {
				return false, apperr.Wrap(apperr.GcloudFailed, "project id "+project+" belongs to someone else", err).
					WithHint("Run `drivekey setup --project <another-id>`.")
			}
			return false, err
		}
		o.Sleep(waitFor(attempt))
	}
}

func retry(ctx context.Context, o Options, f func() (string, error)) (string, error) {
	for attempt := 0; ; attempt++ {
		out, err := f()
		if err == nil || apperr.As(err).Code != apperr.RateLimited || attempt+1 >= o.Attempts {
			return out, err
		}
		if ctx.Err() != nil {
			return "", errors.Join(err, ctx.Err())
		}
		o.Sleep(waitFor(attempt))
	}
}

func waitFor(attempt int) time.Duration {
	d := 10 * time.Second << attempt
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// NewProjectID returns "drivekey-" plus 8 random lowercase letters and digits.
func NewProjectID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "drivekey-" + string(b)
}
