package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

func sheetGrid() [][]string {
	return [][]string{
		{"id", "name", "status"},
		{"1", "Alice", "draft"},
		{"2", "Bob", "draft"},
	}
}

func setArgs(col, key, value string) []string {
	return []string{"S", "--tab", "T", "--key-col", "id", "--key", key, "--col", col, "--value", value}
}

func TestSheetSetWritesOneCell(t *testing.T) {
	g := newFakeGoogle(t)
	g.grid = sheetGrid()
	a := newTestApp(t, g, &strings.Builder{})
	out, err := runSheetSet(context.Background(), a, setArgs("status", "2", "done"))
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["cell"] != "'T'!C3" || m["previous"] != "draft" {
		t.Fatalf("out = %v", m)
	}
	if g.grid[2][2] != "done" || len(g.updates) != 1 {
		t.Fatalf("grid %v updates %v", g.grid, g.updates)
	}
}

func TestSheetSetRefusesWhenColumnInserted(t *testing.T) {
	g := newFakeGoogle(t)
	g.grid = sheetGrid()
	// Another editor inserts a column before "status" right after drivekey read the tab:
	// the key column is untouched, so only the header check can notice.
	g.afterTabRead = func(g *fakeGoogle) {
		for i, row := range g.grid {
			ins := ""
			if i == 0 {
				ins = "notes"
			}
			g.grid[i] = append(row[:2:2], append([]string{ins}, row[2:]...)...)
		}
	}
	a := newTestApp(t, g, &strings.Builder{})
	_, err := runSheetSet(context.Background(), a, setArgs("status", "2", "done"))
	if apperr.As(err).Code != apperr.RowMoved {
		t.Fatalf("err = %v, want row_moved", err)
	}
	if len(g.updates) != 0 {
		t.Fatalf("wrote %v despite the shifted column", g.updates)
	}
}

func TestSheetSetCanChangeTheKeyItself(t *testing.T) {
	g := newFakeGoogle(t)
	g.grid = sheetGrid()
	a := newTestApp(t, g, &strings.Builder{})
	if _, err := runSheetSet(context.Background(), a, setArgs("id", "2", "20")); err != nil {
		t.Fatalf("setting the key column: %v", err)
	}
	if g.grid[2][0] != "20" {
		t.Fatalf("grid %v", g.grid)
	}
}

const folderMimeT = "application/vnd.google-apps.folder"

func changesFixture(t *testing.T) *fakeGoogle {
	g := newFakeGoogle(t)
	g.files["F"] = &drive.File{Id: "F", MimeType: folderMimeT, Parents: []string{"root"}}
	g.addFile("sub", "sub", folderMimeT, "F")
	g.addFile("old", "old.txt", "text/plain", "sub") // existed before tracking started
	return g
}

func changeNames(t *testing.T, s string) []string {
	var out struct{ Changes []Change }
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("output %q: %v", s, err)
	}
	var names []string
	for _, c := range out.Changes {
		names = append(names, c.FileID)
	}
	return names
}

func TestChangesReportsRemovalOfFilePresentAtStart(t *testing.T) {
	g := changesFixture(t)
	var out strings.Builder
	a := newTestApp(t, g, &out)
	ctx := context.Background()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	// After tracking starts, the pre-existing file is deleted forever and an unrelated file changes.
	g.changes = []*drive.Change{
		{FileId: "old", Removed: true, Time: "t1"},
		{FileId: "elsewhere", Time: "t2", File: &drive.File{Id: "elsewhere", Parents: []string{"root"}}},
	}
	g.files["root"] = &drive.File{Id: "root"}
	out.Reset()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	if got := changeNames(t, out.String()); len(got) != 1 || got[0] != "old" {
		t.Fatalf("changes = %v, want [old]", got)
	}
}

func TestChangesNotLostWhenOutputFails(t *testing.T) {
	g := changesFixture(t)
	var out strings.Builder
	a := newTestApp(t, g, &out)
	ctx := context.Background()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	g.addFile("new", "new.txt", "text/plain", "sub")
	g.changes = []*drive.Change{{FileId: "new", Time: "t1", File: g.files["new"]}}

	a.Stdout = failWriter{}
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err == nil {
		t.Fatal("expected the write error")
	}
	a.Stdout = &out
	out.Reset()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	if got := changeNames(t, out.String()); len(got) != 1 || got[0] != "new" {
		t.Fatalf("after a failed write, changes = %v, want [new]", got)
	}
}

func TestChangesReportsRemovalDuringSeeding(t *testing.T) {
	g := changesFixture(t)
	// The pre-existing file is deleted forever right after the start position is taken,
	// before the folder listing would have reached it.
	g.afterStartToken = func(g *fakeGoogle) {
		g.children["sub"] = nil
		delete(g.files, "old")
		g.changes = []*drive.Change{{FileId: "old", Removed: true, Time: "t1"}}
	}
	var out strings.Builder
	a := newTestApp(t, g, &out)
	ctx := context.Background()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	if got := changeNames(t, out.String()); len(got) != 1 || got[0] != "old" {
		t.Fatalf("changes = %v, want [old]", got)
	}
}

// blockingWriter blocks every write until release is closed, like a full pipe nobody reads.
type blockingWriter struct{ release chan struct{} }

func (b blockingWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

func TestStalledWatchDoesNotBlockOtherCommands(t *testing.T) {
	g := changesFixture(t)
	a := newTestApp(t, g, &strings.Builder{})
	ctx := context.Background()
	if err := watchOnce(ctx, a, "F"); err != nil { // starts tracking
		t.Fatal(err)
	}
	g.addFile("new", "new.txt", "text/plain", "sub")
	g.changes = []*drive.Change{{FileId: "new", Time: "t1", File: g.files["new"]}}
	release := make(chan struct{})
	a.Stdout = blockingWriter{release}
	done := make(chan error, 1)
	go func() { done <- watchOnce(ctx, a, "F") }()
	time.Sleep(200 * time.Millisecond) // let watch reach the blocked write
	l, err := a.Dir.Lock(ctx, 2*time.Second)
	if err != nil {
		close(release)
		t.Fatalf("another command could not take the state lock while watch output was stalled: %v", err)
	}
	l.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestChangesReportsRemovalOfFileCreatedDuringSeeding(t *testing.T) {
	g := changesFixture(t)
	// A file appears in the folder after the first listing but before the start position, so
	// its creation is not in the feed; its later removal still has to be reported.
	g.afterList = func(g *fakeGoogle) { g.addFile("late", "late.txt", "text/plain", "F") }
	var out strings.Builder
	a := newTestApp(t, g, &out)
	ctx := context.Background()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	g.changes = []*drive.Change{{FileId: "late", Removed: true, Time: "t1"}}
	out.Reset()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	if got := changeNames(t, out.String()); len(got) != 1 || got[0] != "late" {
		t.Fatalf("changes = %v, want [late]", got)
	}
}

// loginAs records account as the logged-in user, as `drivekey login` does.
func loginAs(t *testing.T, a *App, account string) {
	t.Helper()
	if err := a.Dir.SaveConfig(state.Config{Account: account, Project: "p", SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
}

func TestChangesFeedNotRecreatedAfterLogout(t *testing.T) {
	g := changesFixture(t)
	a := newTestApp(t, g, &strings.Builder{})
	loginAs(t, a, "a@example.com")
	ctx := context.Background()
	// A first poll is in flight (tracking not yet stored) when the user logs out.
	p, err := pollChanges(ctx, a, "F", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Dir.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Dir.ChangesFile()); !os.IsNotExist(err) {
		t.Fatalf("change feed recreated after logout: %v", err)
	}
}

func TestChangesResetWinsOverConcurrentPoll(t *testing.T) {
	g := changesFixture(t)
	var out strings.Builder
	a := newTestApp(t, g, &out)
	loginAs(t, a, "a@example.com")
	ctx := context.Background()
	if _, err := runChanges(ctx, a, []string{"--folder", "F"}); err != nil {
		t.Fatal(err)
	}
	// A normal poll and a reset both read the same stored feed; the normal poll commits first.
	normal, err := pollChanges(ctx, a, "F", false)
	if err != nil {
		t.Fatal(err)
	}
	g.addFile("fresh", "fresh.txt", "text/plain", "F") // only the reset's listing sees it
	reset, err := pollChanges(ctx, a, "F", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := normal.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reset.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// An old poll committing after the reset must not overwrite it either.
	if err := normal.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := loadFeeds(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if fd := all[feedKey("F")]; fd == nil || !fd.Known["fresh"] {
		t.Fatalf("reset was lost: %+v", fd)
	}
}
