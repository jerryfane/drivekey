package gapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/jerryfane/drivekey/internal/apperr"
)

func newTS(t *testing.T, now *time.Time, fetches *int, life time.Duration) *TokenSource {
	t.Helper()
	return &TokenSource{
		Fetch: func(context.Context) (string, error) {
			*fetches++
			return "tok" + string(rune('0'+*fetches)), nil
		},
		Lifetime:  func(context.Context, string) (time.Duration, error) { return life, nil },
		CachePath: filepath.Join(t.TempDir(), "token.json"),
		Account:   "a@example.com",
		Now:       func() time.Time { return *now },
	}
}

func TestTokenCache(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fetches := 0
	ts := newTS(t, &now, &fetches, time.Hour)
	ctx := context.Background()
	if tok, _ := ts.Token(ctx); tok != "tok1" {
		t.Fatalf("first token %q", tok)
	}
	now = now.Add(50 * time.Minute)
	if tok, _ := ts.Token(ctx); tok != "tok1" || fetches != 1 {
		t.Fatalf("cached token %q after %d fetches", tok, fetches)
	}
	// A second process reads the disk cache instead of calling gcloud.
	ts2 := newTS(t, &now, &fetches, time.Hour)
	ts2.CachePath = ts.CachePath
	if tok, _ := ts2.Token(ctx); tok != "tok1" || fetches != 1 {
		t.Fatalf("disk cache: %q after %d fetches", tok, fetches)
	}
	// Within the refresh margin the token is replaced.
	now = now.Add(9 * time.Minute)
	if tok, _ := ts.Token(ctx); tok != "tok2" || fetches != 2 {
		t.Fatalf("refresh: %q after %d fetches", tok, fetches)
	}
	// A cached token for another account is not used.
	ts3 := newTS(t, &now, &fetches, time.Hour)
	ts3.CachePath, ts3.Account = ts.CachePath, "b@example.com"
	if tok, _ := ts3.Token(ctx); tok != "tok3" {
		t.Fatalf("other account got %q", tok)
	}
}

type fakeTokens struct {
	n           atomic.Int32
	invalidated atomic.Int32
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	return "t" + string(rune('0'+f.n.Add(1))), nil
}
func (f *fakeTokens) Invalidate() { f.invalidated.Add(1) }

func TestTransportHeadersRetryAndRefresh(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.Header.Get("X-Goog-User-Project") != "proj-1" {
			t.Errorf("missing quota project header")
		}
		switch n {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`)
		case 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			io.WriteString(w, r.Header.Get("Authorization"))
		}
	}))
	defer srv.Close()
	tokens := &fakeTokens{}
	var slept []time.Duration
	hc := &http.Client{Transport: &Transport{Tokens: tokens, QuotaProject: "proj-1", MaxRetries: 5,
		Sleep: func(d time.Duration) { slept = append(slept, d) }}}
	req, _ := http.NewRequest(http.MethodPut, srv.URL, strings.NewReader("payload"))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || calls.Load() != 4 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls.Load())
	}
	if tokens.invalidated.Load() != 1 || string(body) != "Bearer t4" {
		t.Fatalf("invalidated=%d final auth=%q", tokens.invalidated.Load(), body)
	}
	for i, b := range bodies {
		if b != "payload" {
			t.Fatalf("attempt %d body %q: retries must resend the body", i+1, b)
		}
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want 2 (rate limit and 503; 401 retries immediately)", len(slept))
	}
}

func TestTransportPostRetriesOnlyRejectedRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests) // rejected: safe to resend
		default:
			w.WriteHeader(http.StatusServiceUnavailable) // may have run: must not resend
		}
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &Transport{Tokens: &fakeTokens{}, MaxRetries: 5, Sleep: func(time.Duration) {}}}
	resp, err := hc.Post(srv.URL, "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d, want 503 after 2 calls", resp.StatusCode, calls.Load())
	}
}

func TestTransportDoesNotRetryPermissionDenied(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"errors":[{"reason":"insufficientFilePermissions"}]}}`)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &Transport{Tokens: &fakeTokens{}, MaxRetries: 5, Sleep: func(time.Duration) {}}}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || calls.Load() != 1 || !strings.Contains(string(b), "insufficientFilePermissions") {
		t.Fatalf("status=%d calls=%d body=%q", resp.StatusCode, calls.Load(), b)
	}
}

func TestMapError(t *testing.T) {
	cases := []struct {
		err  *googleapi.Error
		want string
	}{
		{&googleapi.Error{Code: 403, Body: `"reason": "SERVICE_DISABLED"`}, apperr.APIDisabled},
		{&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}, apperr.APIDisabled},
		{&googleapi.Error{Code: 404}, apperr.NotFound},
		{&googleapi.Error{Code: 401}, apperr.NotLoggedIn},
		{&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, apperr.RateLimited},
		{&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "insufficientFilePermissions"}}}, apperr.PermissionDenied},
		{&googleapi.Error{Code: 400, Message: "bad range"}, apperr.GoogleAPI},
	}
	for _, c := range cases {
		if got := MapError(c.err).Code; got != c.want {
			t.Errorf("%+v: got %s, want %s", c.err, got, c.want)
		}
	}
}
