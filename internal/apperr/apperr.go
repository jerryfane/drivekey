// Package apperr defines the machine-readable errors drivekey prints for agents.
package apperr

import (
	"errors"
	"fmt"
)

// Error codes. Agents branch on these, so they are part of the public interface.
const (
	Usage                 = "usage"
	Internal              = "internal"
	GcloudMissing         = "gcloud_missing"
	GcloudFailed          = "gcloud_failed"
	NotLoggedIn           = "not_logged_in"
	NotSetUp              = "not_set_up"
	CloudTermsNotAccepted = "cloud_terms_not_accepted"
	NoLoginPending        = "no_login_pending"
	LoginFailed           = "login_failed"
	APIDisabled           = "api_disabled"
	NotFound              = "not_found"
	PermissionDenied      = "permission_denied"
	RateLimited           = "rate_limited"
	FileExists            = "file_exists"
	ExportRequired        = "export_required"
	UnsupportedExport     = "unsupported_export"
	RowNotFound           = "row_not_found"
	ColumnNotFound        = "column_not_found"
	AmbiguousKey          = "ambiguous_key"
	AmbiguousColumn       = "ambiguous_column"
	RowMoved              = "row_moved"
	GoogleAPI             = "google_api_error"
)

// Error is an error with a stable code, a message and an optional hint for the next step.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Err     error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// New returns an Error with the given code and message.
func New(code, message string) *Error { return &Error{Code: code, Message: message} }

// Newf returns an Error with a formatted message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint returns e with hint set.
func (e *Error) WithHint(hint string) *Error { e.Hint = hint; return e }

// Wrap returns an Error with code and message that wraps err.
func Wrap(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// As converts any error to an *Error, defaulting to Internal.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: Internal, Message: err.Error(), Err: err}
}
