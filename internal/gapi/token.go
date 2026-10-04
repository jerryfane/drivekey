// Package gapi builds authenticated Google API clients that bill the user's own project.
package gapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jerryfane/drivekey/internal/gcloud"
	"github.com/jerryfane/drivekey/internal/state"
)

// refreshMargin is how long before expiry a cached token is replaced.
const refreshMargin = 2 * time.Minute

// fallbackLifetime is assumed when tokeninfo cannot report the expiry.
const fallbackLifetime = 3 * time.Minute

// TokenInfoURL reports an access token's remaining lifetime.
var TokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"

type cachedToken struct {
	AccessToken string    `json:"access_token"`
	Expiry      time.Time `json:"expiry"`
	Account     string    `json:"account"`
}

// TokenSource gets access tokens from `gcloud auth print-access-token` and caches them on disk
// until shortly before they expire, so each drivekey call does not pay gcloud's startup time.
// It never reads gcloud's refresh token.
type TokenSource struct {
	Fetch     func(ctx context.Context) (string, error)
	Lifetime  func(ctx context.Context, token string) (time.Duration, error)
	CachePath string
	Account   string
	Now       func() time.Time

	mu  sync.Mutex
	cur cachedToken
}

// NewTokenSource returns a TokenSource backed by gcloud. The token is always requested for
// account explicitly, never for whichever account happens to be active in gcloud.
func NewTokenSource(r gcloud.Runner, d state.Dir, account string) *TokenSource {
	return &TokenSource{
		Fetch: func(ctx context.Context) (string, error) {
			return r.Run(ctx, "auth", "print-access-token", account)
		},
		Lifetime:  tokenLifetime,
		CachePath: d.TokenCache(),
		Account:   account,
		Now:       time.Now,
	}
}

// Token returns a valid access token.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.Now()
	if t.valid(t.cur, now) {
		return t.cur.AccessToken, nil
	}
	var disk cachedToken
	if err := state.ReadJSON(t.CachePath, &disk); err == nil && t.valid(disk, now) {
		t.cur = disk
		return disk.AccessToken, nil
	}
	tok, err := t.Fetch(ctx)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", errors.New("gcloud returned an empty access token")
	}
	life, err := t.Lifetime(ctx, tok)
	if err != nil || life <= 0 {
		life = fallbackLifetime
	}
	t.cur = cachedToken{AccessToken: tok, Expiry: now.Add(life), Account: t.Account}
	_ = state.WriteJSON(t.CachePath, t.cur)
	return tok, nil
}

// Invalidate drops the cached token, e.g. after a 401.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur = cachedToken{}
	_ = os.Remove(t.CachePath)
}

func (t *TokenSource) valid(c cachedToken, now time.Time) bool {
	return c.AccessToken != "" && c.Account == t.Account && now.Add(refreshMargin).Before(c.Expiry)
}

func tokenLifetime(ctx context.Context, token string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, TokenInfoURL+"?access_token="+url.QueryEscape(token), nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("tokeninfo: HTTP %d", resp.StatusCode)
	}
	var body struct {
		ExpiresIn json.RawMessage `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	var s string
	if err := json.Unmarshal(body.ExpiresIn, &s); err != nil {
		s = string(body.ExpiresIn)
	}
	secs, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("tokeninfo expires_in %q: %w", s, err)
	}
	return time.Duration(secs) * time.Second, nil
}
