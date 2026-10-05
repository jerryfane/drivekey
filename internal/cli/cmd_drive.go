package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/jerryfane/drivekey/internal/apperr"
)

const (
	folderMime      = "application/vnd.google-apps.folder"
	spreadsheetMime = "application/vnd.google-apps.spreadsheet"
	googleAppsMime  = "application/vnd.google-apps."
	fileFields      = "id,name,mimeType,modifiedTime,size,parents,webViewLink,trashed"
)

func init() {
	register("ls", command{usage: "ls [FOLDER_ID] [--query Q] [--trashed]", summary: "List files (in a folder, or matching a Drive query).", run: runLs})
	register("info", command{usage: "info FILE_ID", summary: "Show one file's metadata.", run: runInfo})
	register("get", command{usage: "get FILE_ID [--out PATH|-] [--export FORMAT] [--force]", summary: "Download a file; Google Docs/Sheets/Slides need --export.", run: runGet})
	register("put", command{usage: "put LOCAL_PATH [--parent FOLDER_ID] [--name NAME] [--replace FILE_ID] [--mime TYPE]", summary: "Upload a new file, or replace an existing file's contents.", run: runPut})
	register("mkdir", command{usage: "mkdir NAME [--parent FOLDER_ID]", summary: "Create a folder.", run: runMkdir})
}

// File is drivekey's JSON view of a Drive file.
type File struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	ModifiedTime string   `json:"modifiedTime,omitempty"`
	Size         int64    `json:"size,omitempty"`
	Parents      []string `json:"parents,omitempty"`
	WebViewLink  string   `json:"webViewLink,omitempty"`
	Trashed      bool     `json:"trashed,omitempty"`
}

func fileOf(f *drive.File) File {
	return File{ID: f.Id, Name: f.Name, MimeType: f.MimeType, ModifiedTime: f.ModifiedTime, Size: f.Size,
		Parents: f.Parents, WebViewLink: f.WebViewLink, Trashed: f.Trashed}
}

// quoteQ escapes a value for a Drive query string literal.
func quoteQ(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func runLs(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "ls [FOLDER_ID] [--query Q] [--trashed]"
	fs := newFlags("ls")
	query := fs.String("query", "", "extra Drive query, e.g. \"name contains 'cv'\"")
	trashed := fs.Bool("trashed", false, "include trashed files")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 1, usage); err != nil {
		return nil, err
	}
	var parts []string
	if len(pos) == 1 {
		parts = append(parts, quoteQ(pos[0])+" in parents")
	}
	if !*trashed {
		parts = append(parts, "trashed=false")
	}
	if *query != "" {
		parts = append(parts, "("+*query+")")
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	files := []File{}
	call := c.Drive.Files.List().Q(strings.Join(parts, " and ")).PageSize(1000).
		OrderBy("folder,name").Fields(googleapi.Field("nextPageToken,files(" + fileFields + ")"))
	err = call.Pages(ctx, func(l *drive.FileList) error {
		for _, f := range l.Files {
			files = append(files, fileOf(f))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"files": files}, nil
}

func runInfo(ctx context.Context, a *App, args []string) (any, error) {
	pos, err := parseFlags(newFlags("info"), args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, "info FILE_ID"); err != nil {
		return nil, err
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	f, err := c.Drive.Files.Get(pos[0]).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return fileOf(f), nil
}

// exportExt maps short export names to MIME types. Several MIME types may share a name.
var exportExt = map[string][]string{
	"pdf":  {"application/pdf"},
	"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"ods":  {"application/vnd.oasis.opendocument.spreadsheet", "application/x-vnd.oasis.opendocument.spreadsheet"},
	"csv":  {"text/csv"},
	"tsv":  {"text/tab-separated-values"},
	"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"odt":  {"application/vnd.oasis.opendocument.text"},
	"rtf":  {"application/rtf"},
	"txt":  {"text/plain"},
	"html": {"text/html"},
	"md":   {"text/markdown", "text/x-markdown"},
	"epub": {"application/epub+zip"},
	"zip":  {"application/zip"},
	"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	"odp":  {"application/vnd.oasis.opendocument.presentation"},
	"png":  {"image/png"},
	"jpg":  {"image/jpeg"},
	"svg":  {"image/svg+xml"},
	"json": {"application/vnd.google-apps.script+json"},
}

// resolveExport picks the export MIME type for format among the formats Drive allows.
// It returns the MIME type and the file extension to use.
func resolveExport(format string, allowed []string) (string, string, error) {
	names := exportNames(allowed)
	if format == "" {
		return "", "", apperr.New(apperr.ExportRequired, "this is a Google Docs/Sheets/Slides file; choose an export format").
			WithHint("Add --export with one of: " + strings.Join(names, ", "))
	}
	isAllowed := func(m string) bool {
		for _, a := range allowed {
			if a == m {
				return true
			}
		}
		return false
	}
	if strings.Contains(format, "/") {
		if isAllowed(format) {
			return format, extFor(format), nil
		}
	} else {
		for _, m := range exportExt[strings.ToLower(format)] {
			if isAllowed(m) {
				return m, strings.ToLower(format), nil
			}
		}
	}
	return "", "", apperr.Newf(apperr.UnsupportedExport, "cannot export this file as %q", format).
		WithHint("Use one of: " + strings.Join(names, ", "))
}

func extFor(m string) string {
	for ext, ms := range exportExt {
		for _, x := range ms {
			if x == m {
				return ext
			}
		}
	}
	return ""
}

func exportNames(allowed []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range allowed {
		n := extFor(m)
		if n == "" {
			n = m
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

func runGet(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "get FILE_ID [--out PATH|-] [--export FORMAT] [--force]"
	fs := newFlags("get")
	out := fs.String("out", "", "destination file or directory; - writes the bytes to stdout")
	export := fs.String("export", "", "export format for Google Docs/Sheets/Slides (pdf, xlsx, csv, docx, ...)")
	force := fs.Bool("force", false, "overwrite an existing local file")
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
	f, err := c.Drive.Files.Get(pos[0]).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	name := safeName(f.Name)
	contentMime := f.MimeType
	var body io.ReadCloser
	switch {
	case f.MimeType == folderMime:
		return nil, apperr.New(apperr.Usage, "this is a folder").WithHint("Use `drivekey ls " + f.Id + "`.")
	case strings.HasPrefix(f.MimeType, googleAppsMime):
		about, err := c.Drive.About.Get().Fields("exportFormats").Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		m, ext, err := resolveExport(*export, about.ExportFormats[f.MimeType])
		if err != nil {
			return nil, err
		}
		if ext != "" && !strings.HasSuffix(strings.ToLower(name), "."+ext) {
			name += "." + ext
		}
		resp, err := c.Drive.Files.Export(f.Id, m).Context(ctx).Download()
		if err != nil {
			return nil, err
		}
		body, contentMime = resp.Body, m
	default:
		if *export != "" {
			return nil, apperr.New(apperr.UnsupportedExport, "--export only applies to Google Docs, Sheets and Slides")
		}
		resp, err := c.Drive.Files.Get(f.Id).Context(ctx).Download()
		if err != nil {
			return nil, err
		}
		body = resp.Body
	}
	defer body.Close()

	if *out == "-" {
		_, err := io.Copy(a.Stdout, body)
		return nil, err
	}
	path := name
	if *out != "" {
		path = *out
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			path = filepath.Join(path, name)
		}
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return nil, apperr.Newf(apperr.FileExists, "%s already exists", path).WithHint("Pass --force to overwrite, or --out another path.")
	}
	n, sum, err := writeLocal(path, body)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	return map[string]any{"id": f.Id, "name": f.Name, "mimeType": contentMime, "path": abs, "bytes": n, "sha256": sum}, nil
}

// writeLocal streams r into path via a temp file and returns the byte count and sha256.
func writeLocal(path string, r io.Reader) (int64, string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".part-*")
	if err != nil {
		return 0, "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// convertTargets maps upload content types to the Google format Drive converts them into.
var convertTargets = map[string]string{
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "application/vnd.google-apps.spreadsheet",
	"application/vnd.ms-excel":                       "application/vnd.google-apps.spreadsheet",
	"application/vnd.oasis.opendocument.spreadsheet": "application/vnd.google-apps.spreadsheet",
	"text/csv":                  "application/vnd.google-apps.spreadsheet",
	"text/tab-separated-values": "application/vnd.google-apps.spreadsheet",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "application/vnd.google-apps.document",
	"application/msword":                      "application/vnd.google-apps.document",
	"application/vnd.oasis.opendocument.text": "application/vnd.google-apps.document",
	"application/rtf":                         "application/vnd.google-apps.document",
	"text/plain":                              "application/vnd.google-apps.document",
	"text/html":                               "application/vnd.google-apps.document",
	"text/markdown":                           "application/vnd.google-apps.document",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": "application/vnd.google-apps.presentation",
	"application/vnd.ms-powerpoint":                                             "application/vnd.google-apps.presentation",
	"application/vnd.oasis.opendocument.presentation":                           "application/vnd.google-apps.presentation",
}

// convertTarget returns the Google format for content type ct (parameters such as
// "; charset=utf-8" are ignored).
func convertTarget(ct string) (string, error) {
	base, _, _ := strings.Cut(ct, ";")
	if t, ok := convertTargets[strings.TrimSpace(base)]; ok {
		return t, nil
	}
	return "", apperr.Newf(apperr.Usage, "cannot convert %s into a Google Docs/Sheets/Slides file", ct).
		WithHint("Convertible: spreadsheets (xlsx, xls, ods, csv, tsv), documents (docx, doc, odt, rtf, txt, html, md), presentations (pptx, ppt, odp).")
}

func runPut(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "put LOCAL_PATH [--parent FOLDER_ID] [--name NAME] [--replace FILE_ID] [--mime TYPE] [--convert]"
	fs := newFlags("put")
	parent := fs.String("parent", "", "folder to create the file in (default: My Drive root)")
	name := fs.String("name", "", "name in Drive (default: the local file name)")
	replace := fs.String("replace", "", "replace this file's contents, keeping its id, sharing and links")
	mimeType := fs.String("mime", "", "content type (default: from the file extension)")
	convert := fs.Bool("convert", false, "create a Google Sheet/Doc/Slides file from the upload (xlsx, csv, docx, pptx, ...)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 1, 1, usage); err != nil {
		return nil, err
	}
	if *replace != "" && *parent != "" {
		return nil, apperr.New(apperr.Usage, "--replace and --parent cannot be combined")
	}
	if *replace != "" && *convert {
		return nil, apperr.New(apperr.Usage, "--convert only applies to new files").
			WithHint("Google files are edited in place, e.g. `drivekey sheet write`.")
	}
	local := pos[0]
	fh, err := os.Open(local)
	if err != nil {
		return nil, apperr.Wrap(apperr.Usage, "cannot open "+local, err)
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || st.IsDir() {
		return nil, apperr.Newf(apperr.Usage, "%s is not a regular file", local)
	}
	ct := *mimeType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(local))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	target := ""
	if *convert {
		if target, err = convertTarget(ct); err != nil {
			return nil, err
		}
	}
	c, err := a.Clients(ctx)
	if err != nil {
		return nil, err
	}
	var f *drive.File
	if *replace != "" {
		cur, err := c.Drive.Files.Get(*replace).Fields("id,mimeType").Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(cur.MimeType, googleAppsMime) {
			return nil, apperr.New(apperr.Usage, "refusing to overwrite a Google Docs/Sheets/Slides file with an upload").
				WithHint("Edit Sheets cell by cell with `drivekey sheet write` or `drivekey sheet set`, so other people's edits are kept.")
		}
		meta := &drive.File{}
		if *name != "" {
			meta.Name = *name
		}
		f, err = c.Drive.Files.Update(*replace, meta).Media(fh, googleapi.ContentType(ct)).Fields(fileFields).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
	} else {
		meta := &drive.File{Name: *name, MimeType: target}
		if meta.Name == "" {
			meta.Name = filepath.Base(local)
			if *convert {
				meta.Name = strings.TrimSuffix(meta.Name, filepath.Ext(meta.Name)) // "Roadmap.xlsx" -> "Roadmap"
			}
		}
		if *parent != "" {
			meta.Parents = []string{*parent}
		}
		f, err = c.Drive.Files.Create(meta).Media(fh, googleapi.ContentType(ct)).Fields(fileFields).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
	}
	return fileOf(f), nil
}

func runMkdir(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "mkdir NAME [--parent FOLDER_ID]"
	fs := newFlags("mkdir")
	parent := fs.String("parent", "", "parent folder (default: My Drive root)")
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
	meta := &drive.File{Name: pos[0], MimeType: folderMime}
	if *parent != "" {
		meta.Parents = []string{*parent}
	}
	f, err := c.Drive.Files.Create(meta).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return fileOf(f), nil
}
