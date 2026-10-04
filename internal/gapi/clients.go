package gapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/jerryfane/drivekey/internal/apperr"
)

// Clients are the authenticated API clients for one user and project.
type Clients struct {
	Drive   *drive.Service
	Sheets  *sheets.Service
	Project string
	// Account and Session identify the login the clients act for.
	Account string
	Session string
}

// New builds Drive and Sheets clients whose every call names project as quota project.
func New(ctx context.Context, tokens Tokens, project string) (*Clients, error) {
	hc := &http.Client{Transport: &Transport{Tokens: tokens, QuotaProject: project, MaxRetries: 5}}
	d, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	s, err := sheets.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Clients{Drive: d, Sheets: s, Project: project}, nil
}

// MapError converts Google API errors into apperr codes. Other errors pass through apperr.As.
func MapError(err error) *apperr.Error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae
	}
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return apperr.As(err)
	}
	reasons := reasonsOf(ge)
	has := func(rs ...string) bool {
		for _, r := range rs {
			if reasons[r] || strings.Contains(ge.Body, r) {
				return true
			}
		}
		return false
	}
	msg := ge.Message
	if msg == "" {
		msg = http.StatusText(ge.Code)
	}
	switch {
	case has("SERVICE_DISABLED", "accessNotConfigured"):
		return apperr.Wrap(apperr.APIDisabled, msg, err).
			WithHint("Run `drivekey setup`. If setup just finished, wait a minute for Google to enable the API.")
	case ge.Code == http.StatusUnauthorized:
		return apperr.Wrap(apperr.NotLoggedIn, msg, err).WithHint("Run `drivekey login`.")
	case ge.Code == http.StatusNotFound:
		return apperr.Wrap(apperr.NotFound, msg, err)
	case ge.Code == http.StatusTooManyRequests || has("rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED"):
		return apperr.Wrap(apperr.RateLimited, msg, err).WithHint("Wait a minute and retry.")
	case ge.Code == http.StatusForbidden && has("insufficientFilePermissions", "insufficientPermissions", "forbidden", "PERMISSION_DENIED"):
		return apperr.Wrap(apperr.PermissionDenied, msg, err)
	}
	return apperr.Wrap(apperr.GoogleAPI, msg, err)
}

func reasonsOf(ge *googleapi.Error) map[string]bool {
	out := map[string]bool{}
	for _, e := range ge.Errors {
		out[e.Reason] = true
	}
	for _, d := range ge.Details {
		if m, ok := d.(map[string]any); ok {
			if r, ok := m["reason"].(string); ok {
				out[r] = true
			}
		}
	}
	return out
}
