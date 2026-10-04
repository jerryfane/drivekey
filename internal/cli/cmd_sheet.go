package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/sheets/v4"

	"github.com/jerryfane/drivekey/internal/apperr"
)

func init() {
	register("sheet create", command{usage: "sheet create NAME [--parent FOLDER_ID]", summary: "Create a Google Sheet.", run: runSheetCreate})
	register("sheet tabs", command{usage: "sheet tabs SHEET_ID", summary: "List a spreadsheet's tabs.", run: runSheetTabs})
	register("sheet read", command{usage: "sheet read SHEET_ID RANGE [--raw] [--formulas]", summary: "Read a range, e.g. 'Tab'!A1:D20 or Tab.", run: runSheetRead})
	register("sheet write", command{usage: "sheet write SHEET_ID RANGE --values JSON|- [--raw]", summary: "Write exactly that range; other cells are untouched.", run: runSheetWrite})
	register("sheet append", command{usage: "sheet append SHEET_ID TAB --values JSON|- [--raw]", summary: "Append rows after the last row of a tab.", run: runSheetAppend})
	register("sheet set", command{
		usage:   "sheet set SHEET_ID --tab TAB --key-col HEADER --key VALUE --col HEADER --value V [--header-row N] [--raw]",
		summary: "Set one cell, finding the row by key and the column by header name.",
		run:     runSheetSet,
	})
}

// quoteTab quotes a tab name for A1 notation.
func quoteTab(tab string) string { return "'" + strings.ReplaceAll(tab, "'", "''") + "'" }

// colLetters converts a 0-based column index to A1 letters (0 -> A, 26 -> AA).
func colLetters(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

// a1Start returns the 0-based row and column of the first cell of an A1 range like 'Tab'!B3:D9.
func a1Start(rng string) (row, col int) {
	if i := strings.LastIndex(rng, "!"); i >= 0 {
		rng = rng[i+1:]
	}
	if i := strings.Index(rng, ":"); i >= 0 {
		rng = rng[:i]
	}
	j := 0
	for j < len(rng) && rng[j] >= 'A' && rng[j] <= 'Z' {
		col = col*26 + int(rng[j]-'A'+1)
		j++
	}
	if j > 0 {
		col--
	}
	if n, err := strconv.Atoi(rng[j:]); err == nil && n > 0 {
		row = n - 1
	}
	return row, col
}

func inputOption(raw bool) string {
	if raw {
		return "RAW"
	}
	return "USER_ENTERED"
}

// parseValues reads --values: a JSON array of rows, or a single row; "-" reads stdin.
func parseValues(arg string, stdin io.Reader) ([][]any, error) {
	if arg == "" {
		return nil, apperr.New(apperr.Usage, "--values is required (JSON array of rows, or - for stdin)")
	}
	data := []byte(arg)
	if arg == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		data = b
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, apperr.Wrap(apperr.Usage, "--values must be a JSON array", err)
	}
	if len(raw) == 0 {
		return nil, apperr.New(apperr.Usage, "--values is empty")
	}
	var rows [][]any
	if err := json.Unmarshal(data, &rows); err == nil {
		return rows, nil
	}
	var row []any
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, apperr.Wrap(apperr.Usage, "--values must be an array of rows or one row of scalars", err)
	}
	for _, v := range row {
		if _, ok := v.([]any); ok {
			return nil, apperr.New(apperr.Usage, "--values mixes rows and scalars")
		}
	}
	return [][]any{row}, nil
}

func runSheetCreate(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "sheet create NAME [--parent FOLDER_ID]"
	fs := newFlags("sheet create")
	parent := fs.String("parent", "", "folder to create the sheet in (default: My Drive root)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, usage); err != nil {
		return nil, err
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	meta := &drive.File{Name: pos[0], MimeType: spreadsheetMime}
	if *parent != "" {
		meta.Parents = []string{*parent}
	}
	f, err := c.Drive.Files.Create(meta).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return fileOf(f), nil
}

type tab struct {
	SheetID int64  `json:"sheetId"`
	Title   string `json:"title"`
	Index   int64  `json:"index"`
	Rows    int64  `json:"rows"`
	Columns int64  `json:"columns"`
}

func runSheetTabs(ctx context.Context, a *App, args []string) (any, error) {
	pos, err := parseFlags(newFlags("sheet tabs"), args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, "sheet tabs SHEET_ID"); err != nil {
		return nil, err
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := c.Sheets.Spreadsheets.Get(pos[0]).
		Fields("properties.title,sheets.properties(sheetId,title,index,gridProperties(rowCount,columnCount))").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	tabs := []tab{}
	for _, s := range ss.Sheets {
		p := s.Properties
		t := tab{SheetID: p.SheetId, Title: p.Title, Index: p.Index}
		if p.GridProperties != nil {
			t.Rows, t.Columns = p.GridProperties.RowCount, p.GridProperties.ColumnCount
		}
		tabs = append(tabs, t)
	}
	return map[string]any{"title": ss.Properties.Title, "tabs": tabs}, nil
}

func runSheetRead(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "sheet read SHEET_ID RANGE [--raw] [--formulas]"
	fs := newFlags("sheet read")
	raw := fs.Bool("raw", false, "unformatted values (numbers as numbers)")
	formulas := fs.Bool("formulas", false, "return formulas instead of their results")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 2, 2, usage); err != nil {
		return nil, err
	}
	render := "FORMATTED_VALUE"
	switch {
	case *formulas:
		render = "FORMULA"
	case *raw:
		render = "UNFORMATTED_VALUE"
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	vr, err := c.Sheets.Spreadsheets.Values.Get(pos[0], pos[1]).ValueRenderOption(render).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	values := vr.Values
	if values == nil {
		values = [][]any{}
	}
	return map[string]any{"range": vr.Range, "values": values}, nil
}

func runSheetWrite(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "sheet write SHEET_ID RANGE --values JSON|- [--raw]"
	fs := newFlags("sheet write")
	valuesArg := fs.String("values", "", "JSON array of rows, or - for stdin")
	raw := fs.Bool("raw", false, "store values as-is instead of parsing them like typed input")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 2, 2, usage); err != nil {
		return nil, err
	}
	values, err := parseValues(*valuesArg, a.Stdin)
	if err != nil {
		return nil, err
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.Sheets.Spreadsheets.Values.Update(pos[0], pos[1], &sheets.ValueRange{Values: values}).
		ValueInputOption(inputOption(*raw)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return map[string]any{"updatedRange": resp.UpdatedRange, "updatedRows": resp.UpdatedRows,
		"updatedColumns": resp.UpdatedColumns, "updatedCells": resp.UpdatedCells}, nil
}

func runSheetAppend(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "sheet append SHEET_ID TAB --values JSON|- [--raw]"
	fs := newFlags("sheet append")
	valuesArg := fs.String("values", "", "JSON array of rows, or - for stdin")
	raw := fs.Bool("raw", false, "store values as-is instead of parsing them like typed input")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 2, 2, usage); err != nil {
		return nil, err
	}
	values, err := parseValues(*valuesArg, a.Stdin)
	if err != nil {
		return nil, err
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.Sheets.Spreadsheets.Values.Append(pos[0], quoteTab(pos[1]), &sheets.ValueRange{Values: values}).
		ValueInputOption(inputOption(*raw)).InsertDataOption("INSERT_ROWS").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tableRange": resp.TableRange}
	if u := resp.Updates; u != nil {
		out["updatedRange"], out["updatedRows"], out["updatedCells"] = u.UpdatedRange, u.UpdatedRows, u.UpdatedCells
	}
	return out, nil
}

func cellString(v any) string { return strings.TrimSpace(fmt.Sprint(v)) }

// locateCell finds the sheet row (0-based) whose keyCol value equals key and the column
// index of col, using the header row. values starts at sheet row startRow, column startCol.
func locateCell(values [][]any, startRow, startCol, headerRow int, keyCol, key, col string) (row, keyIdx, colIdx int, err error) {
	hr := headerRow - startRow
	if hr < 0 || hr >= len(values) {
		return 0, 0, 0, apperr.Newf(apperr.ColumnNotFound, "header row %d is empty", headerRow+1)
	}
	headers := values[hr]
	find := func(name string) (int, error) {
		idx := -1
		var all []string
		for i, h := range headers {
			s := cellString(h)
			if s != "" {
				all = append(all, s)
			}
			if s == strings.TrimSpace(name) {
				if idx >= 0 {
					return 0, apperr.Newf(apperr.AmbiguousColumn, "more than one column is named %q", name)
				}
				idx = i
			}
		}
		if idx < 0 {
			return 0, apperr.Newf(apperr.ColumnNotFound, "no column named %q in header row %d", name, headerRow+1).
				WithHint("Columns: " + strings.Join(all, ", "))
		}
		return idx, nil
	}
	ki, err := find(keyCol)
	if err != nil {
		return 0, 0, 0, err
	}
	ci, err := find(col)
	if err != nil {
		return 0, 0, 0, err
	}
	var matches []int
	for i := hr + 1; i < len(values); i++ {
		if ki < len(values[i]) && cellString(values[i][ki]) == strings.TrimSpace(key) {
			matches = append(matches, startRow+i)
		}
	}
	switch len(matches) {
	case 0:
		return 0, 0, 0, apperr.Newf(apperr.RowNotFound, "no row has %s = %q", keyCol, key)
	case 1:
		return matches[0], startCol + ki, startCol + ci, nil
	}
	rows := make([]string, len(matches))
	for i, m := range matches {
		rows[i] = strconv.Itoa(m + 1)
	}
	return 0, 0, 0, apperr.Newf(apperr.AmbiguousKey, "%d rows have %s = %q (rows %s)", len(matches), keyCol, key, strings.Join(rows, ", "))
}

func runSheetSet(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "sheet set SHEET_ID --tab TAB --key-col HEADER --key VALUE --col HEADER --value V [--header-row N] [--raw]"
	fs := newFlags("sheet set")
	tabName := fs.String("tab", "", "tab name")
	keyCol := fs.String("key-col", "", "header of the column that identifies the row")
	key := fs.String("key", "", "value that identifies the row")
	col := fs.String("col", "", "header of the column to set")
	value := fs.String("value", "", "new cell value")
	headerRow := fs.Int("header-row", 1, "row number that holds the headers")
	raw := fs.Bool("raw", false, "store the value as-is instead of parsing it like typed input")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, usage); err != nil {
		return nil, err
	}
	for _, f := range []struct{ name, v string }{{"tab", *tabName}, {"key-col", *keyCol}, {"key", *key}, {"col", *col}} {
		if err := required(f.name, f.v, usage); err != nil {
			return nil, err
		}
	}
	valueSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "value" {
			valueSet = true
		}
	})
	if !valueSet {
		return nil, apperr.New(apperr.Usage, "--value is required (use --value \"\" to clear the cell)")
	}
	if *headerRow < 1 {
		return nil, apperr.New(apperr.Usage, "--header-row must be 1 or more")
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	id, qt := pos[0], quoteTab(*tabName)
	vr, err := c.Sheets.Spreadsheets.Values.Get(id, qt).ValueRenderOption("FORMATTED_VALUE").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	startRow, startCol := a1Start(vr.Range)
	row, ki, ci, err := locateCell(vr.Values, startRow, startCol, *headerRow-1, *keyCol, *key, *col)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s!%s%d", qt, colLetters(ci), row+1)
	previous := ""
	if r := row - startRow; ci-startCol < len(vr.Values[r]) {
		previous = fmt.Sprint(vr.Values[r][ci-startCol])
	}

	// The Sheets API has no conditional write, so re-read the cells that pin the target right
	// before and after writing: the row's key and both column headers. Inserted or deleted rows
	// or columns move at least one of them. When the key column itself is being set, its new
	// value is not compared after the write.
	g := guard{
		{fmt.Sprintf("%s!%s%d", qt, colLetters(ki), *headerRow), *keyCol},
		{fmt.Sprintf("%s!%s%d", qt, colLetters(ci), *headerRow), *col},
		{fmt.Sprintf("%s!%s%d", qt, colLetters(ki), row+1), *key},
	}
	after := g
	if ki == ci {
		after = g[:2]
	}
	ok, err := g.holds(ctx, c.Sheets, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.Newf(apperr.RowMoved, "the sheet changed while reading (row %d, %s = %q); nothing was written", row+1, *keyCol, *key).
			WithHint("Someone is editing the sheet. Run the command again.")
	}
	if _, err := c.Sheets.Spreadsheets.Values.Update(id, target, &sheets.ValueRange{Values: [][]any{{*value}}}).
		ValueInputOption(inputOption(*raw)).Context(ctx).Do(); err != nil {
		return nil, err
	}
	ok, err = after.holds(ctx, c.Sheets, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.Newf(apperr.RowMoved, "the sheet changed while writing: %s was set to %q but may no longer be %s of the row with %s = %q",
			target, *value, *col, *keyCol, *key).
			WithHint(fmt.Sprintf("Check %s; restore it to %q if it now belongs to another row or column.", target, previous))
	}
	return map[string]any{"tab": *tabName, "row": row + 1, "column": *col, "cell": target, "previous": previous, "value": *value}, nil
}

// guard is a set of single cells that must still hold their expected values.
type guard []struct{ cell, want string }

func (g guard) holds(ctx context.Context, s *sheets.Service, id string) (bool, error) {
	ranges := make([]string, len(g))
	for i, c := range g {
		ranges[i] = c.cell
	}
	resp, err := s.Spreadsheets.Values.BatchGet(id).Ranges(ranges...).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if len(resp.ValueRanges) != len(g) {
		return false, nil
	}
	for i, vr := range resp.ValueRanges {
		got := ""
		if len(vr.Values) == 1 && len(vr.Values[0]) == 1 {
			got = cellString(vr.Values[0][0])
		}
		if got != strings.TrimSpace(g[i].want) {
			return false, nil
		}
	}
	return true, nil
}
