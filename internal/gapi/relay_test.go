package gapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

// relayHit is one request as the relay received it.
type relayHit struct {
	Method, Path, Query, Auth, Quota string
}

// fakeRelay records requests and answers them with handle.
type fakeRelay struct {
	mu     sync.Mutex
	hits   []relayHit
	handle func(w http.ResponseWriter, r *http.Request, n int)
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.hits = append(f.hits, relayHit{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Goog-User-Project")})
	n := len(f.hits)
	f.mu.Unlock()
	f.handle(w, r, n)
}

func newRelayClients(t *testing.T, f *fakeRelay) *Clients {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, err := state.NewProfile(srv.URL, "agent-personal", "personal", "my-project-1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewRelay(context.Background(), "personal", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}

// A resumable upload starts on the account's upload service and continues at the session
// URL Google returns, which Keyring passes back unchanged: drivekey must send the chunks to
// the same account's upload service with the session id, never to Google directly.
func TestRelayResumableUploadStaysOnAccountRoute(t *testing.T) {
	const session = "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&upload_id=SESSION-1"
	f := &fakeRelay{}
	f.handle = func(w http.ResponseWriter, r *http.Request, n int) {
		cr := r.Header.Get("Content-Range")
		switch {
		case r.URL.Query().Get("upload_id") == "":
			w.Header().Set("Location", session)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(cr, "/*"): // more chunks follow; Google answers as asked by X-GUploader-No-308
			end := strings.TrimSuffix(strings.SplitN(cr, "-", 2)[1], "/*")
			w.Header().Set("Range", "bytes=0-"+end)
			w.Header().Set("X-Http-Status-Code-Override", "308")
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"new-file","name":"x"}`)
		}
	}
	c := newRelayClients(t, f)
	data := bytes.Repeat([]byte("x"), googleapi.MinUploadChunkSize+10) // two chunks: resumable
	file, err := c.Drive.Files.Create(&drive.File{Name: "x"}).
		Media(bytes.NewReader(data), googleapi.ChunkSize(googleapi.MinUploadChunkSize), googleapi.ContentType("text/plain")).
		Context(context.Background()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if file.Id != "new-file" {
		t.Fatalf("got file %+v", file)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) != 3 {
		t.Fatalf("expected a session start and two chunks, got %+v", f.hits)
	}
	start := f.hits[0]
	if start.Path != "/agent-personal/google-personal-upload/files" || !strings.Contains(start.Query, "uploadType=resumable") {
		t.Fatalf("session start went to %s?%s", start.Path, start.Query)
	}
	for _, h := range f.hits[1:] {
		if h.Path != "/agent-personal/google-personal-upload/files" || !strings.Contains(h.Query, "upload_id=SESSION-1") {
			t.Fatalf("upload chunk went to %s?%s", h.Path, h.Query)
		}
	}
	for _, h := range f.hits {
		if h.Auth != "" {
			t.Fatalf("relay request carried Authorization %q", h.Auth)
		}
		if h.Quota != "my-project-1" {
			t.Fatalf("relay request quota project %q", h.Quota)
		}
	}
}

// A redirect or session URL off the Google API origins must never be followed.
func TestRelayRefusesUnknownOrigin(t *testing.T) {
	f := &fakeRelay{}
	f.handle = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Location", "https://evil.example.com/drive/v3/files/abc?alt=media")
		w.WriteHeader(http.StatusFound)
	}
	c := newRelayClients(t, f)
	_, err := c.Drive.Files.Get("abc").Download()
	if e := MapError(err); e.Code != apperr.RelayRouteRefused {
		t.Fatalf("got %v, want %s", err, apperr.RelayRouteRefused)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) != 1 || f.hits[0].Path != "/agent-personal/google-personal-drive/files/abc" {
		t.Fatalf("hits %+v", f.hits)
	}
}

func TestRelayErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		json   bool
		want   string
	}{
		{"relay cannot reach keyring", 502, "relay: keyring unreachable at 100.1.2.3:7701; calls never fall back to local keys", false, apperr.RelayUnreachable},
		{"relay has no token", 403, "relay: no token for role agent-personal", false, apperr.RelayDenied},
		{"role revoked", 401, "Unauthorized: unknown role token", false, apperr.RelayDenied},
		{"service not granted", 403, "Forbidden: role may not use this service", false, apperr.RelayDenied},
		{"account signed out", 502, "Bad Gateway: gcloud token command failed; sign in again on the keyring host", false, apperr.RelayLoginRequired},
		{"method not granted", 403, "Forbidden: method not allowed for this role", false, apperr.RelayDenied},
		{"daily limit", 429, "Too Many Requests: daily limit reached", false, apperr.RelayDenied},
		{"path refused", 400, `Bad Request: path must be /<service>/<path> in plain printable ASCII, without .., ;, % after decoding, or encoded / . \`, false, apperr.RelayRouteRefused},
		{"google 404 passes through", 404, `{"error":{"code":404,"message":"File not found: abc"}}`, true, apperr.NotFound},
		{"google 403 passes through", 403, `{"error":{"code":403,"message":"no","errors":[{"reason":"insufficientFilePermissions"}]}}`, true, apperr.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRelay{}
			f.handle = func(w http.ResponseWriter, r *http.Request, n int) {
				if tc.json {
					w.Header().Set("Content-Type", "application/json; charset=UTF-8")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				plain(w, tc.status, tc.body)
			}
			c := newRelayClients(t, f)
			_, err := c.Drive.Files.Get("abc").Do()
			if e := MapError(err); e.Code != tc.want {
				t.Fatalf("got %s (%v), want %s", e.Code, err, tc.want)
			}
		})
	}
}

// Google answering 401 means Keyring dropped the token it used and the request was not
// applied. A read is sent once more; a write is not, so the user decides about replaying it.
func TestRelayGoogle401(t *testing.T) {
	unauthorized := func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`)
	}

	f := &fakeRelay{handle: unauthorized}
	c := newRelayClients(t, f)
	// A small upload is one multipart request whose body can be replayed.
	_, err := c.Drive.Files.Create(&drive.File{Name: "x"}).Media(strings.NewReader("data"), googleapi.ContentType("text/plain")).Do()
	if e := MapError(err); e.Code != apperr.RelayTokenRejected {
		t.Fatalf("write: got %v", err)
	}
	if len(f.hits) != 1 {
		t.Fatalf("write was sent %d times", len(f.hits))
	}

	f = &fakeRelay{handle: func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			unauthorized(w, r, n)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"abc"}`)
	}}
	c = newRelayClients(t, f)
	if got, err := c.Drive.Files.Get("abc").Do(); err != nil || got.Id != "abc" {
		t.Fatalf("read after one 401: %v %v", got, err)
	}
}

func TestRelayUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	p, err := state.NewProfile(url, "agent-personal", "personal", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewRelay(context.Background(), "personal", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Drive.Files.Get("abc").Do()
	if e := MapError(err); e.Code != apperr.RelayUnreachable {
		t.Fatalf("got %v", err)
	}
}

func TestRelaySheetsRoute(t *testing.T) {
	f := &fakeRelay{}
	f.handle = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"range":"'My Tab'!A1","values":[["v"]]}`)
	}
	c := newRelayClients(t, f)
	if _, err := c.Sheets.Spreadsheets.Values.Get("SID", "'My Tab'!A1:B2").Do(); err != nil {
		t.Fatal(err)
	}
	if h := f.hits[0]; h.Path != "/agent-personal/google-personal-sheets/spreadsheets/SID/values/%27My%20Tab%27%21A1%3AB2" {
		t.Fatalf("sheets went to %s", h.Path)
	}
}
