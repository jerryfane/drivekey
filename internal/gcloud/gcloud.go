// Package gcloud runs Google's gcloud CLI against drivekey's private config directory.
package gcloud

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/jerryfane/drivekey/internal/apperr"
)

// InstallHint tells the user how to get gcloud.
const InstallHint = "Install the Google Cloud CLI: https://cloud.google.com/sdk/docs/install " +
	"(or set DRIVEKEY_GCLOUD to the gcloud binary)."

// Runner runs gcloud with CLOUDSDK_CONFIG pointing at ConfigDir.
type Runner struct {
	Path      string
	ConfigDir string
}

// Find locates gcloud: $DRIVEKEY_GCLOUD, then PATH.
func Find(configDir string) (Runner, error) {
	if p := os.Getenv("DRIVEKEY_GCLOUD"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return Runner{}, apperr.Wrap(apperr.GcloudMissing, "DRIVEKEY_GCLOUD does not point to a file", err).WithHint(InstallHint)
		}
		return Runner{Path: p, ConfigDir: configDir}, nil
	}
	p, err := exec.LookPath("gcloud")
	if err != nil {
		return Runner{}, apperr.New(apperr.GcloudMissing, "gcloud is not installed or not on PATH").WithHint(InstallHint)
	}
	return Runner{Path: p, ConfigDir: configDir}, nil
}

// Env returns the environment gcloud runs with.
func (r Runner) Env() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLOUDSDK_CONFIG=") || strings.HasPrefix(kv, "CLOUDSDK_CORE_PROJECT=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLOUDSDK_CONFIG="+r.ConfigDir)
}

// Command builds an exec.Cmd for gcloud args.
func (r Runner) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.Env = r.Env()
	return cmd
}

// Run executes gcloud non-interactively and returns trimmed stdout. Failures are classified into apperr codes.
func (r Runner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := r.Command(ctx, args...)
	cmd.Env = append(cmd.Env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", Classify(stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ActiveAccount returns the logged-in account, or "" if none.
func (r Runner) ActiveAccount(ctx context.Context) (string, error) {
	return r.Run(ctx, "auth", "list", "--filter=status:ACTIVE", "--format=value(account)")
}

// Classify maps gcloud stderr to an apperr.Error.
func Classify(stderr string, cause error) *apperr.Error {
	s := stderr
	switch {
	case strings.Contains(s, "Callers must accept Terms of Service"):
		return apperr.Wrap(apperr.CloudTermsNotAccepted, "this Google account has not accepted the Google Cloud terms", cause).
			WithHint("Open https://console.cloud.google.com while logged in as this account, accept the terms, then run `drivekey setup` again.")
	case strings.Contains(s, "RESOURCE_EXHAUSTED"), strings.Contains(s, "Quota exceeded"), strings.Contains(s, "'status': 429"):
		return apperr.Wrap(apperr.RateLimited, "Google rate limit hit", cause).WithHint("Wait a minute and retry.")
	case strings.Contains(s, "You do not currently have an active account"),
		strings.Contains(s, "invalid_grant"),
		strings.Contains(strings.ToLower(s), "reauthentication"),
		strings.Contains(s, "There was a problem refreshing your current auth tokens"):
		return apperr.Wrap(apperr.NotLoggedIn, "not logged in, or the login expired", cause).WithHint("Run `drivekey login`.")
	}
	var ee *exec.Error
	if errors.As(cause, &ee) {
		return apperr.Wrap(apperr.GcloudMissing, "could not run gcloud", cause).WithHint(InstallHint)
	}
	return apperr.Wrap(apperr.GcloudFailed, lastLines(s, 6), cause)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	msg := strings.TrimSpace(strings.Join(lines, "\n"))
	if msg == "" {
		return "gcloud failed"
	}
	return msg
}

// ExtractLoginURL pulls the accounts.google.com URL out of `gcloud auth login --no-launch-browser`
// output. It tolerates the URL being wrapped across lines. Returns "" if not found yet.
func ExtractLoginURL(out string) string {
	i := strings.Index(out, "https://accounts.google.com/")
	if i < 0 {
		return ""
	}
	rest := out[i:]
	end := len(rest)
	for _, stop := range []string{"\n\n", "\r\n\r\n", "Once finished", "Enter "} {
		if j := strings.Index(rest, stop); j >= 0 && j < end {
			end = j
		}
	}
	if end == len(rest) && !strings.Contains(rest, "\n") {
		// URL line not finished yet.
		return ""
	}
	return strings.Join(strings.Fields(rest[:end]), "")
}
