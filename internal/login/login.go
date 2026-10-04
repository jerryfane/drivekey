// Package login runs `gcloud auth login` as a two-step flow agents can drive:
// `drivekey login` prints the URL, `drivekey login --code CODE` finishes it.
//
// A detached helper process owns the waiting gcloud process (which holds the PKCE
// verifier) and talks to the CLI through files in the login directory, so it works
// across separate agent tool calls and on every OS.
package login

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gcloud"
	"github.com/jerryfane/drivekey/internal/state"
)

// HelperArg is the hidden subcommand that runs the helper.
const HelperArg = "__login-helper"

// Timeouts; variables so tests can shorten them.
var (
	CodeWait   = 10 * time.Minute // how long the helper waits for a code
	URLWait    = 90 * time.Second // how long `login` waits for gcloud to print the URL
	ResultWait = 2 * time.Minute  // how long `login --code` waits for gcloud to finish
	poll       = 100 * time.Millisecond
)

// LoginArgs are the gcloud arguments for a Drive-enabled login.
var LoginArgs = []string{"auth", "login", "--enable-gdrive-access", "--no-launch-browser"}

type files struct{ dir string }

func (f files) pid() string    { return filepath.Join(f.dir, "helper.pid") }
func (f files) url() string    { return filepath.Join(f.dir, "url") }
func (f files) code() string   { return filepath.Join(f.dir, "code") }
func (f files) result() string { return filepath.Join(f.dir, "result.json") }
func (f files) log() string    { return filepath.Join(f.dir, "helper.log") }

// Result is the outcome the helper writes when gcloud exits.
type Result struct {
	OK      bool          `json:"ok"`
	Account string        `json:"account,omitempty"`
	Error   *apperr.Error `json:"error,omitempty"`
}

// Start launches a fresh helper and returns the login URL.
func Start(ctx context.Context, d state.Dir, r gcloud.Runner) (string, error) {
	if err := d.Ensure(); err != nil {
		return "", err
	}
	f := files{d.LoginDir()}
	stopHelper(f)
	for _, p := range []string{f.url(), f.code(), f.result(), f.log(), f.pid()} {
		_ = os.Remove(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate drivekey binary: %w", err)
	}
	logf, err := os.OpenFile(f.log(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer logf.Close()
	cmd := exec.Command(exe, HelperArg)
	cmd.Env = append(os.Environ(), "DRIVEKEY_HOME="+d.Root, "DRIVEKEY_GCLOUD="+r.Path)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start login helper: %w", err)
	}
	// Reap the helper if it exits while this process is still alive, so a zombie is
	// never mistaken for a waiting login. Once this process exits, init adopts it.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(URLWait)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(f.url()); err == nil && len(b) > 0 {
			return string(b), nil
		}
		if res, ok := readResult(f); ok {
			return "", resultError(res)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
	stopHelper(f)
	return "", apperr.New(apperr.LoginFailed, "gcloud did not print a login URL in time").
		WithHint("See " + f.log() + ", then run `drivekey login` again.")
}

// Submit hands the code to the waiting helper and returns the logged-in account.
func Submit(ctx context.Context, d state.Dir, code string) (string, error) {
	f := files{d.LoginDir()}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", apperr.New(apperr.Usage, "empty code")
	}
	if res, ok := readResult(f); ok {
		return "", resultError(res)
	}
	if !helperAlive(f) {
		return "", apperr.New(apperr.NoLoginPending, "no login is waiting for a code").WithHint("Run `drivekey login` first.")
	}
	if err := state.WriteFileAtomic(f.code(), []byte(code)); err != nil {
		return "", err
	}
	deadline := time.Now().Add(ResultWait)
	for time.Now().Before(deadline) {
		if res, ok := readResult(f); ok {
			if !res.OK {
				return "", resultError(res)
			}
			cfg, err := d.LoadConfig()
			if err != nil {
				return "", err
			}
			if cfg.Account != res.Account {
				// A different account invalidates the previous project choice.
				cfg = state.Config{Account: res.Account}
			}
			if err := d.SaveConfig(cfg); err != nil {
				return "", err
			}
			_ = os.Remove(d.TokenCache())
			return res.Account, nil
		}
		if !helperAlive(f) {
			if res, ok := readResult(f); ok {
				return "", resultError(res)
			}
			return "", apperr.New(apperr.LoginFailed, "login helper exited without a result").
				WithHint("See " + f.log() + ", then run `drivekey login` again.")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
	return "", apperr.New(apperr.LoginFailed, "gcloud did not finish the login in time").
		WithHint("Run `drivekey login` again.")
}

// Pending reports whether a helper is waiting for a code.
func Pending(d state.Dir) bool {
	f := files{d.LoginDir()}
	_, done := readResult(f)
	return !done && helperAlive(f)
}

// Interactive runs gcloud login attached to the terminal.
func Interactive(ctx context.Context, d state.Dir, r gcloud.Runner) (string, error) {
	if err := d.Ensure(); err != nil {
		return "", err
	}
	cmd := r.Command(ctx, LoginArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", apperr.Wrap(apperr.LoginFailed, "gcloud login failed", err)
	}
	account, err := r.ActiveAccount(ctx)
	if err != nil {
		return "", err
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.Account != account {
		cfg = state.Config{Account: account}
	}
	_ = os.Remove(d.TokenCache())
	return account, d.SaveConfig(cfg)
}

// RunHelper is the body of the detached helper process.
func RunHelper(d state.Dir, r gcloud.Runner) error {
	f := files{d.LoginDir()}
	if err := os.WriteFile(f.pid(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(f.pid())
	res := runGcloudLogin(d, r, f)
	return state.WriteJSON(f.result(), res)
}

func runGcloudLogin(d state.Dir, r gcloud.Runner, f files) Result {
	ctx, cancel := context.WithTimeout(context.Background(), CodeWait+ResultWait)
	defer cancel()
	cmd := r.Command(ctx, LoginArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failed(apperr.Wrap(apperr.LoginFailed, "gcloud stdin", err))
	}
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return failed(gcloud.Classify("", err))
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Phase 1: wait for the URL, then for a code.
	urlWritten := false
	deadline := time.Now().Add(CodeWait)
	for {
		if !urlWritten {
			if u := gcloud.ExtractLoginURL(out.String()); u != "" {
				if err := state.WriteFileAtomic(f.url(), []byte(u)); err != nil {
					_ = cmd.Process.Kill()
					return failed(apperr.Wrap(apperr.Internal, "write url", err))
				}
				urlWritten = true
			}
		}
		if code, err := os.ReadFile(f.code()); err == nil {
			_ = os.Remove(f.code())
			_, _ = io.WriteString(stdin, strings.TrimSpace(string(code))+"\n")
			_ = stdin.Close()
			break
		}
		select {
		case err := <-exited:
			return failed(apperr.Wrap(apperr.LoginFailed, "gcloud exited before a code was entered: "+tail(out.String()), err))
		case <-time.After(poll):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return failed(apperr.New(apperr.LoginFailed, "no code was entered within "+CodeWait.String()).
				WithHint("Run `drivekey login` again."))
		}
	}

	// Phase 2: wait for gcloud to exchange the code.
	select {
	case err := <-exited:
		if err != nil {
			return failed(apperr.Wrap(apperr.LoginFailed, "Google rejected the code: "+tail(out.String()), err).
				WithHint("Run `drivekey login` again and paste the newest code exactly."))
		}
	case <-time.After(ResultWait):
		_ = cmd.Process.Kill()
		return failed(apperr.New(apperr.LoginFailed, "gcloud did not finish after the code was entered"))
	}
	actx, acancel := context.WithTimeout(context.Background(), time.Minute)
	defer acancel()
	account, err := r.ActiveAccount(actx)
	if err != nil {
		return failed(apperr.As(err))
	}
	if account == "" {
		return failed(apperr.New(apperr.LoginFailed, "gcloud reported success but no active account"))
	}
	return Result{OK: true, Account: account}
}

func failed(e *apperr.Error) Result { return Result{Error: e} }

func resultError(r Result) error {
	if r.Error != nil {
		return r.Error
	}
	return apperr.New(apperr.LoginFailed, "login failed")
}

func readResult(f files) (Result, bool) {
	var r Result
	if err := state.ReadJSON(f.result(), &r); err != nil {
		return Result{}, false
	}
	return r, true
}

func helperPID(f files) int {
	b, err := os.ReadFile(f.pid())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func helperAlive(f files) bool {
	pid := helperPID(f)
	return pid > 0 && processAlive(pid)
}

func stopHelper(f files) {
	if pid := helperPID(f); pid > 0 && processAlive(pid) {
		killProcessGroup(pid)
		deadline := time.Now().Add(5 * time.Second)
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(poll)
		}
	}
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < 4; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.Contains(l, "accounts.google.com") {
			continue
		}
		keep = append([]string{l}, keep...)
	}
	return strings.Join(keep, " ")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
