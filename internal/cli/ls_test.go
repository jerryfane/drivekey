package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func ls(t *testing.T, a *App, args ...string) (files []File, truncated bool) {
	t.Helper()
	out, err := runLs(context.Background(), a, args)
	if err != nil {
		t.Fatalf("drivekey ls %s: %v", strings.Join(args, " "), err)
	}
	m := out.(map[string]any)
	if m["truncated"] == true && m["hint"] == nil {
		t.Fatalf("drivekey ls %s: truncated without a hint", strings.Join(args, " "))
	}
	return m["files"].([]File), m["truncated"] == true
}

// Listing a whole large Drive stops at the limit and says more files match,
// instead of paging through all of them (more than 30,000 files took
// minutes, seen live).
func TestLsStopsAtTheLimit(t *testing.T) {
	g := newFakeGoogle(t)
	for i := range 2500 {
		g.addFile(fmt.Sprintf("f%05d", i), fmt.Sprintf("file %d", i), "text/plain", "root")
	}
	a := newTestApp(t, g, &strings.Builder{})

	if files, more := ls(t, a); len(files) != 1000 || !more || g.listPages != 1 {
		t.Fatalf("ls: %d files, truncated %v, %d pages", len(files), more, g.listPages)
	}
	if files, more := ls(t, a, "--limit", "10"); len(files) != 10 || !more {
		t.Fatalf("ls --limit 10: %d files, truncated %v", len(files), more)
	}
	g.listPages = 0
	if files, more := ls(t, a, "--limit", "0"); len(files) != 2500 || more || g.listPages != 3 {
		t.Fatalf("ls --limit 0: %d files, truncated %v, %d pages", len(files), more, g.listPages)
	}
	// Exactly the limit, nothing more: not truncated.
	if files, more := ls(t, a, "--limit", "2500"); len(files) != 2500 || more {
		t.Fatalf("ls --limit 2500 of 2500: %d files, truncated %v", len(files), more)
	}
	if _, err := runLs(context.Background(), a, []string{"--limit", "-1"}); err == nil {
		t.Fatal("ls --limit -1 accepted")
	}
}

// A fullText search is not sorted: Google refuses to (400), which made the
// first search for a sheet fail and the agent fall back to listing everything.
func TestLsFullTextSearchIsNotSorted(t *testing.T) {
	g := newFakeGoogle(t)
	g.addFile("s1", "mamma expenses", "application/vnd.google-apps.spreadsheet", "root")
	a := newTestApp(t, g, &strings.Builder{})
	if files, _ := ls(t, a, "--query", "fullText contains 'mamma'"); len(files) != 1 {
		t.Fatalf("fullText search: %d files", len(files))
	}
}
