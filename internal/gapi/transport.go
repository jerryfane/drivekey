package gapi

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Tokens supplies access tokens; *TokenSource implements it.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// Transport adds the access token and the quota project to every request, refreshes the
// token once on 401, and retries rate-limit and server errors when the body can be replayed.
type Transport struct {
	Base         http.RoundTripper
	Tokens       Tokens
	QuotaProject string
	MaxRetries   int
	Sleep        func(time.Duration)
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	sleep := t.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	refreshed := false
	for attempt := 0; ; attempt++ {
		r := req.Clone(req.Context())
		if attempt > 0 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = b
		}
		tok, err := t.Tokens.Token(req.Context())
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		if t.QuotaProject != "" {
			r.Header.Set("X-Goog-User-Project", t.QuotaProject)
		}
		resp, err := base.RoundTrip(r)
		if err != nil {
			if replayable && attempt < t.MaxRetries && req.Context().Err() == nil {
				sleep(backoff(attempt, ""))
				continue
			}
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshed && replayable {
			drain(resp)
			t.Tokens.Invalidate()
			refreshed = true
			continue
		}
		if !replayable || attempt >= t.MaxRetries || !retryable(resp) {
			return resp, nil
		}
		retryAfter := resp.Header.Get("Retry-After")
		drain(resp)
		sleep(backoff(attempt, retryAfter))
	}
}

// retryable reports whether resp is a transient failure. It may replace resp.Body.
func retryable(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		s := string(b)
		return strings.Contains(s, "rateLimitExceeded") || strings.Contains(s, "userRateLimitExceeded") ||
			strings.Contains(s, "RATE_LIMIT_EXCEEDED")
	}
	return false
}

func backoff(attempt int, retryAfter string) time.Duration {
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 && secs <= 120 {
		return time.Duration(secs) * time.Second
	}
	d := time.Second << attempt
	if d > 32*time.Second {
		d = 32 * time.Second
	}
	return d + time.Duration(rand.Int64N(int64(time.Second)))
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}
