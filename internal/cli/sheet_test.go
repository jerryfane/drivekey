package cli

import (
	"strings"
	"testing"

	"github.com/jerryfane/drivekey/internal/apperr"
)

func TestColLettersRoundTrip(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		got := colLetters(i)
		if got != want {
			t.Errorf("colLetters(%d) = %s, want %s", i, got, want)
		}
		if _, c := a1Start("'T'!" + got + "1"); c != i {
			t.Errorf("a1Start(%s1) col = %d, want %d", got, c, i)
		}
	}
}

func TestA1Start(t *testing.T) {
	cases := map[string][2]int{
		"Foglio1!A1:Z1000":    {0, 0},
		"'My Tab'!C5:F9":      {4, 2},
		"'Tab!with!bangs'!B2": {1, 1},
		"'Ta''b'!AA10:AB20":   {9, 26},
	}
	for in, want := range cases {
		r, c := a1Start(in)
		if r != want[0] || c != want[1] {
			t.Errorf("a1Start(%q) = %d,%d want %d,%d", in, r, c, want[0], want[1])
		}
	}
}

func grid() [][]any {
	return [][]any{
		{"id", "name", "status"},
		{"1", "Alice", "draft"},
		{"2", "Bob"}, // short row: status cell empty
		{" 3 ", "Carol", "done"},
		{"4", "Dan", "draft"},
		{"4", "Dup", "draft"},
	}
}

func TestLocateCell(t *testing.T) {
	row, ki, ci, err := locateCell(grid(), 0, 0, 0, "id", "2", "status")
	if err != nil || row != 2 || ki != 0 || ci != 2 {
		t.Fatalf("got row=%d key=%d col=%d err=%v", row, ki, ci, err)
	}
	// Keys and headers are compared after trimming spaces.
	if row, _, _, err := locateCell(grid(), 0, 0, 0, " id ", "3", "name"); err != nil || row != 3 {
		t.Fatalf("trim: row=%d err=%v", row, err)
	}
	// Offsets: values starting at sheet row 5, column C.
	row, ki, ci, err = locateCell(grid(), 4, 2, 4, "id", "1", "name")
	if err != nil || row != 5 || ki != 2 || ci != 3 {
		t.Fatalf("offset: row=%d key=%d col=%d err=%v", row, ki, ci, err)
	}
}

func TestLocateCellErrors(t *testing.T) {
	cases := []struct {
		keyCol, key, col, code string
	}{
		{"id", "9", "status", apperr.RowNotFound},
		{"id", "4", "status", apperr.AmbiguousKey},
		{"id", "1", "missing", apperr.ColumnNotFound},
		{"nope", "1", "status", apperr.ColumnNotFound},
	}
	for _, c := range cases {
		_, _, _, err := locateCell(grid(), 0, 0, 0, c.keyCol, c.key, c.col)
		if got := apperr.As(err).Code; got != c.code {
			t.Errorf("%+v: code = %s, want %s", c, got, c.code)
		}
	}
	_, _, _, err := locateCell(grid(), 0, 0, 0, "id", "4", "status")
	if !strings.Contains(err.Error(), "rows 5, 6") {
		t.Errorf("ambiguous error should name the rows: %v", err)
	}
	dup := [][]any{{"id", "x", "x"}, {"1", "a", "b"}}
	if _, _, _, err := locateCell(dup, 0, 0, 0, "id", "1", "x"); apperr.As(err).Code != apperr.AmbiguousColumn {
		t.Errorf("duplicate header: %v", err)
	}
}

func TestParseValues(t *testing.T) {
	rows, err := parseValues(`[["a",1],["b",true]]`, nil)
	if err != nil || len(rows) != 2 || rows[1][1] != true {
		t.Fatalf("rows: %v %v", rows, err)
	}
	row, err := parseValues(`["a","b"]`, nil)
	if err != nil || len(row) != 1 || len(row[0]) != 2 {
		t.Fatalf("single row: %v %v", row, err)
	}
	stdin, err := parseValues("-", strings.NewReader(`[["x"]]`))
	if err != nil || stdin[0][0] != "x" {
		t.Fatalf("stdin: %v %v", stdin, err)
	}
	for _, bad := range []string{"", "[]", `{"a":1}`, `["a",["b"]]`} {
		if _, err := parseValues(bad, nil); apperr.As(err).Code != apperr.Usage {
			t.Errorf("parseValues(%q) err = %v, want usage", bad, err)
		}
	}
}

func TestConvertTarget(t *testing.T) {
	cases := map[string]string{
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": spreadsheetMime,
		"text/csv; charset=utf-8": spreadsheetMime,
		"TEXT/CSV":                spreadsheetMime,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "application/vnd.google-apps.document",
	}
	for ct, want := range cases {
		if got, err := convertTarget(ct); err != nil || got != want {
			t.Errorf("convertTarget(%q) = %q, %v; want %q", ct, got, err, want)
		}
	}
	if _, err := convertTarget("application/pdf"); apperr.As(err).Code != apperr.Usage {
		t.Errorf("pdf must not be convertible: %v", err)
	}
}

func TestResolveExport(t *testing.T) {
	allowed := []string{"application/pdf", "text/csv", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}
	m, ext, err := resolveExport("CSV", allowed)
	if err != nil || m != "text/csv" || ext != "csv" {
		t.Fatalf("csv: %s %s %v", m, ext, err)
	}
	if _, _, err := resolveExport("", allowed); apperr.As(err).Code != apperr.ExportRequired ||
		!strings.Contains(apperr.As(err).Hint, "csv, pdf, xlsx") {
		t.Fatalf("missing export: %v", err)
	}
	if _, _, err := resolveExport("docx", allowed); apperr.As(err).Code != apperr.UnsupportedExport {
		t.Fatalf("docx: %v", err)
	}
	if m, _, err := resolveExport("application/pdf", allowed); err != nil || m != "application/pdf" {
		t.Fatalf("mime: %s %v", m, err)
	}
}
