package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

// relayFor serves one Keyring account through a fake relay: requests for role's
// google-<account>-{drive,sheets} services reach g at the Google path the service fronts.
// Every other request, and any request carrying a credential, fails the test.
type relayFor struct {
	t             *testing.T
	role, account string
	g             *fakeGoogle
	mu            sync.Mutex
	paths         []string
}

func (f *relayFor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_relay/health" {
		_, _ = w.Write([]byte("ok: keyring reachable at test\n"))
		return
	}
	if r.Header.Get("Authorization") != "" {
		f.t.Errorf("relay request carried Authorization")
	}
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	prefix := "/" + f.role + "/google-" + f.account + "-"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	svc, tail, _ := strings.Cut(rest, "/")
	if !ok {
		f.t.Errorf("relay: unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch svc {
	case "drive":
		r.URL.Path = "/drive/v3/" + tail
	case "sheets":
		r.URL.Path = "/v4/" + tail
	default:
		f.t.Errorf("relay: unexpected service %s", svc)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	r.URL.RawPath = ""
	f.g.ServeHTTP(w, r)
}

// relayApp returns an App whose state directory has a logged-in direct login and one relay
// profile per given relay, named after its account.
func relayApp(t *testing.T, relays ...*relayFor) (*App, *strings.Builder) {
	t.Helper()
	dir := state.Dir{Root: filepath.Join(t.TempDir(), "dk")}
	if err := dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := dir.SaveConfig(state.Config{Account: "a@example.com", Session: "s1", Project: "p", SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	a := &App{Dir: dir, Stdout: out, Stderr: &strings.Builder{}, Stdin: strings.NewReader("")}
	for _, r := range relays {
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)
		mustRun(t, a, "profile", "add", r.account, "--relay", srv.URL, "--role", r.role, "--account", r.account)
	}
	out.Reset()
	return a, out
}

// run runs one drivekey command line on a fresh App over the same state, like a new process.
func run(a *App, args ...string) (map[string]any, *apperr.Error) {
	out, errOut := &strings.Builder{}, &strings.Builder{}
	b := &App{Dir: a.Dir, Stdout: out, Stderr: errOut, Stdin: strings.NewReader(""), Profile: a.Profile}
	if code := b.Run(context.Background(), args); code != 0 {
		var e struct{ Error *apperr.Error }
		_ = json.Unmarshal([]byte(errOut.String()), &e)
		if e.Error == nil {
			e.Error = &apperr.Error{Code: "unparsed", Message: errOut.String()}
		}
		return nil, e.Error
	}
	m := map[string]any{}
	if s := strings.TrimSpace(out.String()); s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, &apperr.Error{Code: "unparsed", Message: s}
		}
	}
	return m, nil
}

func mustRun(t *testing.T, a *App, args ...string) map[string]any {
	t.Helper()
	m, e := run(a, args...)
	if e != nil {
		t.Fatalf("drivekey %s: %s: %s", strings.Join(args, " "), e.Code, e.Message)
	}
	return m
}

func newRelayFor(t *testing.T, account string) *relayFor {
	g := newFakeGoogle(t)
	g.files["F"] = &drive.File{Id: "F", Name: "F", MimeType: folderMime}
	g.addFile("old", "old.txt", "text/plain", "F")
	return &relayFor{t: t, role: "agent-" + account, account: account, g: g}
}

// Once a relay profile exists, a command without an explicit profile must not quietly use
// this machine's own login, and an unknown profile name must not fall back to anything.
func TestRelayProfileSelectionNeverFallsBack(t *testing.T) {
	r := newRelayFor(t, "personal")
	a, _ := relayApp(t, r)
	if _, e := run(a, "info", "old"); e == nil || e.Code != apperr.ProfileRequired {
		t.Fatalf("no profile: got %+v", e)
	}
	if _, e := run(a, "--profile", "business", "info", "old"); e == nil || e.Code != apperr.ProfileNotFound {
		t.Fatalf("unknown profile: got %+v", e)
	}
	if len(r.paths) != 0 {
		t.Fatalf("relay was called: %v", r.paths)
	}
	m := mustRun(t, a, "--profile", "personal", "info", "old")
	if m["id"] != "old" || len(r.paths) != 1 || r.paths[0] != "/agent-personal/google-personal-drive/files/old" {
		t.Fatalf("info = %v via %v", m, r.paths)
	}
}

// login, setup and logout manage this machine's own Google login; for a relay profile they
// must refuse instead of touching (or deleting) local login state.
func TestRelayProfileRefusesLocalLoginCommands(t *testing.T) {
	a, _ := relayApp(t, newRelayFor(t, "personal"))
	for _, cmd := range []string{"login", "setup", "logout"} {
		if _, e := run(a, "--profile", "personal", cmd); e == nil || e.Code != apperr.RelayMode {
			t.Fatalf("%s: got %+v", cmd, e)
		}
	}
	cfg, err := a.Dir.LoadConfig()
	if err != nil || cfg.Account != "a@example.com" || !cfg.SetupComplete {
		t.Fatalf("direct login state changed: %+v %v", cfg, err)
	}
}

// Each relay profile has its own change feeds: one account's position never advances or
// answers for another account, nor for the direct login, and a redefined profile starts over.
func TestRelayChangeFeedsArePerProfile(t *testing.T) {
	personal, business := newRelayFor(t, "personal"), newRelayFor(t, "business")
	a, _ := relayApp(t, personal, business)
	for _, p := range []string{"personal", "business"} {
		if m := mustRun(t, a, "--profile", p, "changes", "--folder", "F"); m["initialized"] != true {
			t.Fatalf("%s first poll: %v", p, m)
		}
	}
	if _, err := os.Stat(a.Dir.ChangesFile()); !os.IsNotExist(err) {
		t.Fatalf("direct feed file written: %v", err)
	}
	personal.g.changes = []*drive.Change{{FileId: "new", Time: "t", File: &drive.File{Id: "new", Name: "new.txt", Parents: []string{"F"}}}}
	m := mustRun(t, a, "--profile", "personal", "changes", "--folder", "F")
	if ch, _ := m["changes"].([]any); len(ch) != 1 {
		t.Fatalf("personal changes: %v", m)
	}
	if m := mustRun(t, a, "--profile", "business", "changes", "--folder", "F"); len(m["changes"].([]any)) != 0 || m["initialized"] != nil {
		t.Fatalf("business changes: %v", m)
	}
	// Redefining the profile (here: the same account behind a new relay) must not reuse its feeds.
	srv := httptest.NewServer(personal)
	t.Cleanup(srv.Close)
	mustRun(t, a, "profile", "remove", "personal")
	mustRun(t, a, "profile", "add", "personal", "--relay", srv.URL, "--role", "agent-personal", "--account", "personal")
	if m := mustRun(t, a, "--profile", "personal", "changes", "--folder", "F"); m["initialized"] != true {
		t.Fatalf("redefined profile reused a feed: %v", m)
	}
}

func TestRelaySheetSet(t *testing.T) {
	r := newRelayFor(t, "personal")
	r.g.grid = sheetGrid()
	a, _ := relayApp(t, r)
	a.Profile = "personal"
	m := mustRun(t, a, append([]string{"sheet", "set"}, setArgs("status", "2", "done")...)...)
	if m["cell"] != "'T'!C3" || r.g.grid[2][2] != "done" {
		t.Fatalf("out %v grid %v", m, r.g.grid)
	}
	for _, p := range r.paths {
		if !strings.HasPrefix(p, "/agent-personal/google-personal-sheets/") {
			t.Fatalf("sheets request went to %s", p)
		}
	}
}

// A folder in a shared drive must be listable, readable and tracked like one in My Drive,
// including through a relay profile.
func TestSharedDriveFolder(t *testing.T) {
	r := newRelayFor(t, "business")
	g := r.g
	g.files["SF"] = &drive.File{Id: "SF", Name: "Team", MimeType: folderMime, DriveId: "SD"}
	g.addFile("doc", "plan.txt", "text/plain", "SF")
	g.files["doc"].DriveId = "SD"
	a, _ := relayApp(t, r)
	a.Profile = "business"

	m := mustRun(t, a, "ls", "SF")
	if files, _ := m["files"].([]any); len(files) != 1 || files[0].(map[string]any)["id"] != "doc" {
		t.Fatalf("ls SF = %v", m)
	}
	if m := mustRun(t, a, "info", "doc"); m["driveId"] != "SD" {
		t.Fatalf("info doc = %v", m)
	}
	if m := mustRun(t, a, "changes", "--folder", "SF"); m["initialized"] != true {
		t.Fatalf("first poll = %v", m)
	}
	g.addFile("new", "new.txt", "text/plain", "SF")
	g.files["new"].DriveId = "SD"
	g.changes = []*drive.Change{{FileId: "new", Time: "t1", File: g.files["new"]}}
	m = mustRun(t, a, "changes", "--folder", "SF")
	if ch, _ := m["changes"].([]any); len(ch) != 1 || ch[0].(map[string]any)["fileId"] != "new" {
		t.Fatalf("changes in shared drive folder = %v", m)
	}
}

// status reports which mode is in use: the relay's health and the Google account it reaches
// for a relay profile, and the local login otherwise.
func TestStatusModes(t *testing.T) {
	a, _ := relayApp(t)
	if m := mustRun(t, a, "status"); m["mode"] != "direct" {
		t.Fatalf("status without profiles = %v", m)
	}
	a, _ = relayApp(t, newRelayFor(t, "personal"))
	if m := mustRun(t, a, "status"); m["mode"] != "unselected" {
		t.Fatalf("status with profiles, none chosen = %v", m)
	}
	m := mustRun(t, a, "--profile", "personal", "status")
	if m["mode"] != "relay" || m["relay"] != "ok" || m["google_user"] != "relayed@example.com" {
		t.Fatalf("relay status = %v", m)
	}
}
