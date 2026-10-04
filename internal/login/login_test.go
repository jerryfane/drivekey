//go:build !windows

package login

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gcloud"
	"github.com/jerryfane/drivekey/internal/state"
)

// TestMain lets the test binary act as the detached helper, as the real binary does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		d := state.Dir{Root: os.Getenv("DRIVEKEY_HOME")}
		r := gcloud.Runner{Path: os.Getenv("DRIVEKEY_GCLOUD"), ConfigDir: d.GcloudConfig()}
		if err := RunHelper(d, r); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeGcloud mimics `gcloud auth login --no-launch-browser`: it prints a wrapped URL on
// stderr, reads a code, and only accepts "good-code".
const fakeGcloud = `#!/bin/sh
case "$1 $2" in
"auth login")
  echo "Go to the following link in your browser, and complete the sign-in prompts:" >&2
  echo "" >&2
  echo "    https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=1.apps" >&2
  echo "ercontent.com&scope=openid+drive&state=abc" >&2
  echo "" >&2
  printf "Once finished, enter the verification code provided in your browser: " >&2
  read code
  if [ "$code" = "good-code" ]; then
    echo "user@example.com" > "$CLOUDSDK_CONFIG/active"
    echo "You are now logged in as [user@example.com]." >&2
    exit 0
  fi
  echo "ERROR: (gcloud.auth.login) invalid_grant: Malformed auth code." >&2
  exit 1;;
"auth list")
  cat "$CLOUDSDK_CONFIG/active" 2>/dev/null
  exit 0;;
esac
exit 3
`

func setup(t *testing.T) (state.Dir, gcloud.Runner) {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "gcloud")
	if err := os.WriteFile(bin, []byte(fakeGcloud), 0o755); err != nil {
		t.Fatal(err)
	}
	d := state.Dir{Root: filepath.Join(tmp, "home")}
	return d, gcloud.Runner{Path: bin, ConfigDir: d.GcloudConfig()}
}

func TestTwoStepLogin(t *testing.T) {
	d, r := setup(t)
	ctx := context.Background()
	url, err := Start(ctx, d, r)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := "https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=1.appsercontent.com&scope=openid+drive&state=abc"
	if url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
	if !Pending(d) {
		t.Fatal("login should be pending")
	}
	account, err := Submit(ctx, d, "  good-code\n")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if account != "user@example.com" {
		t.Fatalf("account = %q", account)
	}
	cfg, err := d.LoadConfig()
	if err != nil || cfg.Account != "user@example.com" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if Pending(d) {
		t.Fatal("login should no longer be pending")
	}
}

func TestWrongCodeThenRetry(t *testing.T) {
	d, r := setup(t)
	ctx := context.Background()
	if _, err := Start(ctx, d, r); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(ctx, d, "bad-code")
	if e := apperr.As(err); e.Code != apperr.LoginFailed || !strings.Contains(e.Message, "invalid_grant") {
		t.Fatalf("err = %v, want login_failed mentioning invalid_grant", err)
	}
	// A fresh login after a failure works.
	if _, err := Start(ctx, d, r); err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(ctx, d, "good-code"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestSubmitWithoutLogin(t *testing.T) {
	d, _ := setup(t)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(context.Background(), d, "good-code")
	if e := apperr.As(err); e.Code != apperr.NoLoginPending {
		t.Fatalf("err = %v, want no_login_pending", err)
	}
}

func TestRestartStopsOldHelper(t *testing.T) {
	d, r := setup(t)
	ctx := context.Background()
	if _, err := Start(ctx, d, r); err != nil {
		t.Fatal(err)
	}
	old := helperPID(files{d.LoginDir()})
	if _, err := Start(ctx, d, r); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(old) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(old) {
		t.Fatalf("old helper %d still running", old)
	}
	if _, err := Submit(ctx, d, "good-code"); err != nil {
		t.Fatalf("Submit to new helper: %v", err)
	}
}
