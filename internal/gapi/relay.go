package gapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

// The Google endpoints the API clients are built for. In relay mode no request ever goes to
// them: the transport rewrites each one onto the profile's Keyring service for that endpoint.
const (
	driveEndpoint  = "https://www.googleapis.com/drive/v3/"
	sheetsEndpoint = "https://sheets.googleapis.com/"
)

// googleRoute is one Google origin and path prefix that a Keyring service fronts.
type googleRoute struct {
	host, prefix string
	service      func(state.Profile) string
}

// googleRoutes are the only Google URLs relay mode will send anywhere. Upload session URLs
// (resumable uploads) and redirects that Google hands back are mapped through the same table,
// so they stay on the same account's relay services; any other URL fails closed.
var googleRoutes = []googleRoute{
	{"www.googleapis.com", "/upload/drive/v3", state.Profile.UploadService},
	{"www.googleapis.com", "/drive/v3", state.Profile.DriveService},
	{"sheets.googleapis.com", "/v4", state.Profile.SheetsService},
}

// Relay sends requests through a Keyring relay profile. It never holds or adds a credential.
type Relay struct {
	Name    string
	Profile state.Profile
}

// route maps a Google API URL onto the relay. Anything that is not one of googleRoutes is refused.
func (r *Relay) route(u *url.URL) (*url.URL, error) {
	refuse := func(why string) error {
		return apperr.Newf(apperr.RelayRouteRefused, "refusing to send %s through relay profile %q: %s",
			redactURL(u), r.Name, why).
			WithHint("Relay mode only reaches the Drive, Drive upload and Sheets APIs of the profile's own Keyring services.")
	}
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return nil, refuse("not a Google API origin")
	}
	host := strings.ToLower(u.Hostname())
	p := u.EscapedPath()
	for _, gr := range googleRoutes {
		if host != gr.host || !strings.HasPrefix(p, gr.prefix+"/") {
			continue
		}
		rest := strings.TrimPrefix(p, gr.prefix)
		if rest == "/" {
			return nil, refuse("no API path")
		}
		target, err := url.Parse(r.Profile.Relay + "/" + r.Profile.Role + "/" + gr.service(r.Profile) + rest)
		if err != nil {
			return nil, refuse("unparseable path")
		}
		target.RawQuery = u.RawQuery
		return target, nil
	}
	return nil, refuse("not a Drive, Drive upload or Sheets API URL")
}

// redactURL drops the query, which can carry an upload session id.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery, c.ForceQuery, c.Fragment, c.User = "", false, "", nil
	return c.String()
}

// check turns a Keyring or relay refusal into an apperr. Both answer in plain text: the relay
// prefixes "relay: ", Keyring "<status text>: ". Google's own errors are JSON and pass through.
// It may replace resp.Body.
func (r *Relay) check(resp *http.Response) *apperr.Error {
	if resp.StatusCode < 400 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	text := strings.TrimSpace(string(b))
	p := r.Profile
	who := "role " + p.Role + " (profile " + r.Name + ")"
	if note, ok := strings.CutPrefix(text, "relay: "); ok {
		switch {
		case resp.StatusCode == http.StatusBadGateway:
			return apperr.Newf(apperr.RelayUnreachable, "Keyring relay: %s", note).
				WithHint("The relay at " + p.Relay + " cannot reach the Keyring host. Check `curl " + p.Relay + "/_relay/health`. drivekey never falls back to a local login.")
		case strings.HasPrefix(note, "no token for role"):
			return apperr.Newf(apperr.RelayDenied, "Keyring relay has no token for %s", who).
				WithHint("Request one with `keyring request --role " + p.Role + " ...`, have the owner approve it, then restart keyring-relay.")
		}
		return apperr.Newf(apperr.RelayError, "Keyring relay: %s", note)
	}
	note, ok := strings.CutPrefix(text, http.StatusText(resp.StatusCode)+": ")
	if !ok && !(resp.StatusCode == http.StatusForbidden && text == "forbidden") {
		return nil
	}
	if !ok {
		note = "this machine's address is not allowed"
	}
	lower := strings.ToLower(note)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return apperr.Newf(apperr.RelayDenied, "Keyring refused %s: %s", who, note).
			WithHint("The role is unknown, revoked or expired. The owner approves or renews it in the Keyring dashboard.")
	case resp.StatusCode == http.StatusForbidden:
		return apperr.Newf(apperr.RelayDenied, "Keyring refused %s: %s", who, note).
			WithHint("The owner grants role " + p.Role + " the services " + p.DriveService() + ", " + p.UploadService() + " and " + p.SheetsService() + " (with the methods and paths drivekey uses) in the Keyring dashboard.")
	case resp.StatusCode == http.StatusTooManyRequests:
		return apperr.Newf(apperr.RelayDenied, "Keyring refused %s: %s", who, note).
			WithHint("The role's Keyring daily limit is used up; it resets daily, or the owner raises it.")
	case resp.StatusCode == http.StatusBadRequest:
		return apperr.Newf(apperr.RelayRouteRefused, "Keyring refused the request: %s", note).
			WithHint("Keyring forwards only printable ASCII paths without '%', ';' or '\\'. A Sheets tab whose name has other characters cannot be addressed by name through Keyring.")
	case strings.Contains(lower, "sign in again") || strings.Contains(lower, "set up the account") ||
		strings.Contains(lower, "identity or expiry mismatch"):
		return apperr.Newf(apperr.RelayLoginRequired, "Keyring cannot get a Google token for account %q: %s", p.Account, note).
			WithHint("The owner must sign in to this Google account again on the Keyring host. Nothing on this machine can fix it.")
	}
	return apperr.Newf(apperr.RelayError, "Keyring: %s", note).
		WithHint("Retry later. A write that failed this way may already have been applied; check before sending it again.")
}

// tokenRejected is returned when Google answers 401 to a relayed call: Keyring dropped the
// token it used, and the request was not applied.
func (r *Relay) tokenRejected() error {
	return apperr.Newf(apperr.RelayTokenRejected, "Google rejected the Keyring token for account %q; the request was not applied", r.Profile.Account).
		WithHint("Run the command again. If it fails the same way, the owner must sign in to this account again on the Keyring host.")
}

// NewRelay builds Drive and Sheets clients that reach Google only through profile p's Keyring
// services. No credential, gcloud or local login is involved.
func NewRelay(ctx context.Context, name string, p state.Profile, base http.RoundTripper) (*Clients, error) {
	t := &Transport{Base: base, Relay: &Relay{Name: name, Profile: p}, QuotaProject: p.QuotaProject, MaxRetries: 5}
	hc := &http.Client{Transport: t}
	d, err := drive.NewService(ctx, option.WithHTTPClient(hc), option.WithEndpoint(driveEndpoint))
	if err != nil {
		return nil, err
	}
	s, err := sheets.NewService(ctx, option.WithHTTPClient(hc), option.WithEndpoint(sheetsEndpoint))
	if err != nil {
		return nil, err
	}
	return &Clients{Drive: d, Sheets: s, Project: p.QuotaProject, Account: p.Identity(), Session: p.ID, Profile: name}, nil
}
