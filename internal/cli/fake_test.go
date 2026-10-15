package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/jerryfane/drivekey/internal/gapi"
	"github.com/jerryfane/drivekey/internal/state"
)

// fakeGoogle is a minimal in-memory Drive + Sheets backend for one spreadsheet tab and a folder tree.
type fakeGoogle struct {
	mu sync.Mutex
	t  *testing.T

	// Sheets: one tab "T" whose grid starts at A1.
	grid    [][]string
	updates []string // A1 ranges written
	// afterTabRead runs once after the whole-tab read, to simulate a concurrent editor.
	afterTabRead func(g *fakeGoogle)

	// Drive.
	children map[string][]*drive.File // parent id -> children
	files    map[string]*drive.File
	changes  []*drive.Change // changes after token 1
	// afterStartToken runs once after the start position is handed out; afterList runs once
	// after the first folder listing.
	afterStartToken, afterList func(g *fakeGoogle)
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	return &fakeGoogle{t: t, children: map[string][]*drive.File{}, files: map[string]*drive.File{}}
}

func (g *fakeGoogle) addFile(id, name, mime, parent string) {
	f := &drive.File{Id: id, Name: name, MimeType: mime, Parents: []string{parent}}
	g.files[id] = f
	g.children[parent] = append(g.children[parent], f)
}

func (g *fakeGoogle) cell(a1 string) string {
	r, c := a1Start(a1)
	if r < len(g.grid) && c < len(g.grid[r]) {
		return g.grid[r][c]
	}
	return ""
}

func writeBody(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/values:batchGet"):
		var vrs []*sheets.ValueRange
		for _, rng := range r.URL.Query()["ranges"] {
			vr := &sheets.ValueRange{Range: rng}
			if v := g.cell(rng); v != "" {
				vr.Values = [][]any{{v}}
			}
			vrs = append(vrs, vr)
		}
		writeBody(w, &sheets.BatchGetValuesResponse{ValueRanges: vrs})
	case strings.Contains(p, "/values/") && r.Method == http.MethodGet:
		rows := make([][]any, len(g.grid))
		for i, row := range g.grid {
			for _, v := range row {
				rows[i] = append(rows[i], v)
			}
		}
		writeBody(w, &sheets.ValueRange{Range: "T!A1:Z1000", Values: rows})
		if g.afterTabRead != nil {
			f := g.afterTabRead
			g.afterTabRead = nil
			f(g)
		}
	case strings.HasSuffix(p, "/values:batchUpdate") && r.Method == http.MethodPost:
		var req sheets.BatchUpdateValuesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := &sheets.BatchUpdateValuesResponse{}
		for _, vr := range req.Data {
			row, col := a1Start(vr.Range)
			for len(g.grid) <= row {
				g.grid = append(g.grid, nil)
			}
			for len(g.grid[row]) <= col {
				g.grid[row] = append(g.grid[row], "")
			}
			g.grid[row][col] = vr.Values[0][0].(string)
			g.updates = append(g.updates, vr.Range)
			resp.Responses = append(resp.Responses, &sheets.UpdateValuesResponse{UpdatedRange: vr.Range, UpdatedCells: 1})
		}
		writeBody(w, resp)
	case p == "/drive/v3/about":
		writeBody(w, &drive.About{User: &drive.User{EmailAddress: "relayed@example.com"}})
	case p == "/drive/v3/changes/startPageToken":
		writeBody(w, &drive.StartPageToken{StartPageToken: "1"})
		if g.afterStartToken != nil {
			f := g.afterStartToken
			g.afterStartToken = nil
			f(g)
		}
	case p == "/drive/v3/changes":
		// Like Google: a shared drive's items are only reported from that drive's own change
		// log (driveId set), and only to callers that declare shared-drive support.
		q := r.URL.Query()
		var list []*drive.Change
		if q.Get("pageToken") == "1" {
			for _, ch := range g.changes {
				if g.visible(r, ch.FileId) && g.driveOf(ch.FileId) == q.Get("driveId") {
					list = append(list, ch)
				}
			}
		}
		writeBody(w, &drive.ChangeList{Changes: list, NewStartPageToken: "2"})
	case p == "/drive/v3/files" && r.Method == http.MethodGet:
		q := r.URL.Query()
		before, _, _ := strings.Cut(q.Get("q"), " in parents")
		parent := strings.Trim(before, "'")
		files := []*drive.File{}
		for _, f := range g.children[parent] {
			if f.DriveId == "" || (g.visible(r, f.Id) && q.Get("includeItemsFromAllDrives") == "true") {
				files = append(files, f)
			}
		}
		writeBody(w, &drive.FileList{Files: files})
		if g.afterList != nil {
			f := g.afterList
			g.afterList = nil
			f(g)
		}
	case strings.HasPrefix(p, "/drive/v3/drives/") && r.Method == http.MethodGet:
		writeBody(w, &drive.Drive{Id: strings.TrimPrefix(p, "/drive/v3/drives/")})
	case strings.HasPrefix(p, "/drive/v3/files/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(p, "/drive/v3/files/")
		f, ok := g.files[id]
		if !ok || !g.visible(r, id) {
			w.WriteHeader(http.StatusNotFound)
			writeBody(w, map[string]any{"error": map[string]any{"code": 404, "message": "File not found: " + id}})
			return
		}
		writeBody(w, f)
	default:
		g.t.Errorf("fake google: unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (g *fakeGoogle) driveOf(id string) string {
	if f := g.files[id]; f != nil {
		return f.DriveId
	}
	return ""
}

// visible reports whether Google shows file id to r: shared-drive items only to callers
// that pass supportsAllDrives=true.
func (g *fakeGoogle) visible(r *http.Request, id string) bool {
	return g.driveOf(id) == "" || r.URL.Query().Get("supportsAllDrives") == "true"
}

type staticTokens struct{}

func (staticTokens) Token(context.Context) (string, error) { return "t", nil }
func (staticTokens) Invalidate()                           {}

// newTestApp returns an App wired to a fake Google backend, with stdout going to out.
func newTestApp(t *testing.T, g *fakeGoogle, out *strings.Builder) *App {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &gapi.Transport{Tokens: staticTokens{}, QuotaProject: "p"}}
	ctx := context.Background()
	d, err := drive.NewService(ctx, option.WithHTTPClient(hc), option.WithEndpoint(srv.URL+"/drive/v3/"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := sheets.NewService(ctx, option.WithHTTPClient(hc), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	dir := state.Dir{Root: filepath.Join(t.TempDir(), "dk")}
	if err := dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := dir.SaveConfig(state.Config{Account: "a@example.com", Session: "s1", Project: "p", SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return &App{Dir: dir, Stdout: out, Stderr: &strings.Builder{}, Stdin: strings.NewReader(""),
		clients: &gapi.Clients{Drive: d, Sheets: s, Project: "p", Account: "a@example.com", Session: "s1"}}
}

// failWriter fails every write, like a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
