// Package cli implements the drivekey command line. Every command prints JSON on stdout;
// failures print {"error": {...}} on stderr and exit non-zero.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gapi"
	"github.com/jerryfane/drivekey/internal/gcloud"
	"github.com/jerryfane/drivekey/internal/login"
	"github.com/jerryfane/drivekey/internal/state"
)

// Version is set at build time with -ldflags "-X github.com/jerryfane/drivekey/internal/cli.Version=...".
var Version = "dev"

// App carries what commands need.
type App struct {
	Dir    state.Dir
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// Profile is the explicitly selected relay profile ("direct" for the local login), from
	// --profile or $DRIVEKEY_PROFILE. Empty means no selection was made.
	Profile string

	clients *gapi.Clients
}

type command struct {
	usage   string
	summary string
	// locked commands run while holding the state lock, because they read and rewrite shared state.
	locked bool
	run    func(ctx context.Context, a *App, args []string) (any, error)
}

// lockWait bounds how long a command waits for another drivekey command to finish.
const lockWait = 5 * time.Minute

var commands = map[string]command{}

func register(name string, c command) { commands[name] = c }

// Main runs drivekey with args (without the program name) and returns the exit code.
func Main(args []string) int {
	a := &App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Profile: os.Getenv("DRIVEKEY_PROFILE")}
	d, err := state.Default()
	if err != nil {
		return a.fail(err)
	}
	a.Dir = d
	if len(args) == 1 && args[0] == login.HelperArg {
		r, err := gcloud.Find(d.GcloudConfig())
		if err != nil {
			return a.fail(err)
		}
		if err := login.RunHelper(d, r); err != nil {
			fmt.Fprintln(a.Stderr, err)
			return 1
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return a.Run(ctx, args)
}

// Run dispatches one command. A leading --profile NAME selects a relay profile.
func (a *App) Run(ctx context.Context, args []string) int {
	for len(args) > 0 && (args[0] == "--profile" || args[0] == "-profile" ||
		strings.HasPrefix(args[0], "--profile=") || strings.HasPrefix(args[0], "-profile=")) {
		if _, v, ok := strings.Cut(args[0], "="); ok {
			a.Profile, args = v, args[1:]
			continue
		}
		if len(args) < 2 {
			return a.fail(apperr.New(apperr.Usage, "--profile needs a profile name").WithHint("usage: drivekey --profile NAME COMMAND ..."))
		}
		a.Profile, args = args[1], args[2:]
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.usage(a.Stdout)
		return 0
	}
	name, rest := args[0], args[1:]
	if name == "sheet" || name == "profile" {
		if len(rest) == 0 {
			a.usage(a.Stderr)
			return 2
		}
		name, rest = name+" "+rest[0], rest[1:]
	}
	c, ok := commands[name]
	if !ok {
		return a.fail(apperr.Newf(apperr.Usage, "unknown command %q", name).WithHint("Run `drivekey help`."))
	}
	if c.locked {
		l, err := a.Dir.Lock(ctx, lockWait)
		if err != nil {
			return a.fail(err)
		}
		defer l.Unlock()
	}
	out, err := c.run(ctx, a, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(a.Stdout, "usage: drivekey %s\n", c.usage)
			return 0
		}
		return a.fail(err)
	}
	if out == nil {
		return 0
	}
	if err := writeJSON(a.Stdout, out); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) fail(err error) int {
	e := gapi.MapError(err)
	_ = writeJSON(a.Stderr, map[string]any{"error": e})
	if e.Code == apperr.Usage {
		return 2
	}
	return 1
}

func writeCompactJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func mapErr(err error) error { return gapi.MapError(err) }

func apperrCode(err error) string { return gapi.MapError(err).Code }

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "drivekey %s: give an agent access to your own Google Drive and Sheets.\n\nCommands:\n", Version)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  drivekey %-60s %s\n", commands[n].usage, commands[n].summary)
	}
	fmt.Fprintln(w, "\nOutput is JSON. Errors go to stderr as {\"error\": {\"code\", \"message\", \"hint\"}}.")
	fmt.Fprintln(w, "State lives in $DRIVEKEY_HOME (default ~/.config/drivekey).")
	fmt.Fprintln(w, "Relay mode: `drivekey --profile NAME COMMAND` (or $DRIVEKEY_PROFILE=NAME) calls Google through")
	fmt.Fprintln(w, "that Keyring relay profile; once any profile exists, every account command needs a profile")
	fmt.Fprintln(w, "(`--profile direct` selects this machine's own login).")
}

// gcloud finds the gcloud binary bound to drivekey's private config.
func (a *App) gcloud() (gcloud.Runner, error) { return gcloud.Find(a.Dir.GcloudConfig()) }

// relayProfile resolves the selected profile. It returns a nil profile for direct mode: no
// relay profiles exist and none was selected, or "direct" was selected. Once any relay
// profile exists the choice must be explicit, and an unknown name never falls back.
func (a *App) relayProfile() (string, *state.Profile, error) {
	profiles, err := a.Dir.LoadProfiles()
	if err != nil {
		return "", nil, err
	}
	switch a.Profile {
	case state.DirectProfile:
		return "", nil, nil
	case "":
		if len(profiles) == 0 {
			return "", nil, nil
		}
		return "", nil, apperr.Newf(apperr.ProfileRequired, "relay profiles exist (%s); choose one explicitly", strings.Join(profileNames(profiles), ", ")).
			WithHint("Run `drivekey --profile NAME ...` or set DRIVEKEY_PROFILE=NAME. `--profile direct` uses this machine's own login.")
	}
	p, ok := profiles[a.Profile]
	if !ok {
		return "", nil, apperr.Newf(apperr.ProfileNotFound, "no relay profile named %q", a.Profile).
			WithHint("Run `drivekey profile list`, or add it with `drivekey profile add`.")
	}
	return a.Profile, &p, nil
}

func profileNames(p state.Profiles) []string {
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Clients returns API clients for the selected relay profile, or for the configured
// account and project in direct mode.
func (a *App) Clients(ctx context.Context) (*gapi.Clients, error) {
	if a.clients != nil {
		return a.clients, nil
	}
	name, p, err := a.relayProfile()
	if err != nil {
		return nil, err
	}
	if p != nil {
		c, err := gapi.NewRelay(ctx, name, *p, nil)
		if err != nil {
			return nil, err
		}
		a.clients = c
		return c, nil
	}
	cfg, err := a.Dir.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Account == "" {
		return nil, apperr.New(apperr.NotLoggedIn, "not logged in").WithHint("Run `drivekey login`.")
	}
	if !cfg.SetupComplete || cfg.Project == "" {
		return nil, apperr.New(apperr.NotSetUp, "setup has not finished").WithHint("Run `drivekey setup`.")
	}
	r, err := a.gcloud()
	if err != nil {
		return nil, err
	}
	c, err := gapi.New(ctx, gapi.NewTokenSource(r, a.Dir, cfg.Account), cfg.Project)
	if err != nil {
		return nil, err
	}
	// Record whose login these clients act for, so state they produce is attributed to
	// that login and not to whatever config says later.
	c.Account, c.Session = cfg.Account, cfg.Session
	a.clients = c
	return c, nil
}

// parseFlags parses fs, allowing flags before, between and after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, apperr.New(apperr.Usage, err.Error())
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// positional checks the number of positional arguments.
func positional(pos []string, min, max int, usage string) error {
	if len(pos) < min || len(pos) > max {
		return apperr.New(apperr.Usage, "wrong number of arguments").WithHint("usage: drivekey " + usage)
	}
	return nil
}

func newFlags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }

func required(name, value, usage string) error {
	if strings.TrimSpace(value) == "" {
		return apperr.Newf(apperr.Usage, "--%s is required", name).WithHint("usage: drivekey " + usage)
	}
	return nil
}
