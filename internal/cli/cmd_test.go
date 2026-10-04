package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
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
