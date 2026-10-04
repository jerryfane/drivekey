package setup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

// fakeRunner simulates gcloud's project commands.
type fakeRunner struct {
	account    string
	projects   map[string]string // id -> lifecycle state
	createErrs []error           // returned by successive creates before succeeding
	enabled    []string
	creates    int
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	switch strings.Join(args[:2], " ") {
	case "auth list":
		return f.account, nil
	case "projects describe":
		if st, ok := f.projects[args[2]]; ok {
			return st, nil
		}
		return "", apperr.New(apperr.GcloudFailed, "not found or permission denied")
	case "projects create":
		f.creates++
		if len(f.createErrs) > 0 {
			err := f.createErrs[0]
			f.createErrs = f.createErrs[1:]
			return "", err
		}
		f.projects[args[2]] = "ACTIVE"
		return "", nil
	case "services enable":
		f.enabled = append(f.enabled, args[2:len(args)-2]...)
		return "", nil
	}
	return "", apperr.Newf(apperr.GcloudFailed, "unexpected %v", args)
}

func newDir(t *testing.T) state.Dir {
	d := state.Dir{Root: filepath.Join(t.TempDir(), "dk")}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	return d
}

func noSleep(time.Duration) {}

func TestSetupCreatesOnceAndIsIdempotent(t *testing.T) {
	d := newDir(t)
	f := &fakeRunner{account: "u@example.com", projects: map[string]string{}}
	res, err := Run(context.Background(), d, f, Options{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !strings.HasPrefix(res.Project, "drivekey-") || len(res.Project) != len("drivekey-")+8 {
		t.Fatalf("result %+v", res)
	}
	cfg, _ := d.LoadConfig()
	if !cfg.SetupComplete || cfg.Project != res.Project || cfg.Account != "u@example.com" {
		t.Fatalf("config %+v", cfg)
	}
	res2, err := Run(context.Background(), d, f, Options{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.Project != res.Project || f.creates != 1 {
		t.Fatalf("second run %+v, creates=%d", res2, f.creates)
	}
}

func TestSetupTermsNotAccepted(t *testing.T) {
	d := newDir(t)
	f := &fakeRunner{account: "u@example.com", projects: map[string]string{},
		createErrs: []error{apperr.New(apperr.CloudTermsNotAccepted, "terms")}}
	_, err := Run(context.Background(), d, f, Options{Sleep: noSleep})
	if apperr.As(err).Code != apperr.CloudTermsNotAccepted {
		t.Fatalf("err = %v", err)
	}
	first, _ := d.LoadConfig()
	if first.SetupComplete {
		t.Fatal("setup must not be complete")
	}
	// After the user accepts the terms, setup reuses the same project id.
	res, err := Run(context.Background(), d, f, Options{Sleep: noSleep})
	if err != nil || res.Project != first.Project {
		t.Fatalf("retry: %+v %v (want project %s)", res, err, first.Project)
	}
}

func TestSetupRetriesRateLimit(t *testing.T) {
	d := newDir(t)
	rl := apperr.New(apperr.RateLimited, "429")
	f := &fakeRunner{account: "u@example.com", projects: map[string]string{}, createErrs: []error{rl, rl}}
	var waits []time.Duration
	res, err := Run(context.Background(), d, f, Options{Sleep: func(d time.Duration) { waits = append(waits, d) }})
	if err != nil || !res.Created || f.creates != 3 || len(waits) != 2 {
		t.Fatalf("res=%+v err=%v creates=%d waits=%v", res, err, f.creates, waits)
	}
}

func TestSetupWaitsForAPIPropagation(t *testing.T) {
	d := newDir(t)
	f := &fakeRunner{account: "u@example.com", projects: map[string]string{}}
	calls := 0
	verify := func(context.Context, string) error {
		calls++
		if calls < 3 {
			return apperr.New(apperr.APIDisabled, "not yet")
		}
		return nil
	}
	if _, err := Run(context.Background(), d, f, Options{Sleep: noSleep, Verify: verify}); err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestSetupNewAccountForgetsOldProject(t *testing.T) {
	d := newDir(t)
	if err := d.SaveConfig(state.Config{Account: "old@example.com", Project: "drivekey-old", SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{account: "new@example.com", projects: map[string]string{"drivekey-old": "ACTIVE"}}
	res, err := Run(context.Background(), d, f, Options{Sleep: noSleep})
	if err != nil || res.Project == "drivekey-old" || !res.Created {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSetupNotLoggedIn(t *testing.T) {
	d := newDir(t)
	f := &fakeRunner{projects: map[string]string{}}
	if _, err := Run(context.Background(), d, f, Options{Sleep: noSleep}); apperr.As(err).Code != apperr.NotLoggedIn {
		t.Fatalf("err = %v", err)
	}
}
