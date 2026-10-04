package gcloud

import (
	"errors"
	"testing"

	"github.com/jerryfane/drivekey/internal/apperr"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name, stderr, want string
	}{
		{"terms", "ERROR: (gcloud.projects.create) Operation [x] failed: 9: Callers must accept Terms of Service\n- '@type': type.googleapis.com/google.rpc.PreconditionFailure", apperr.CloudTermsNotAccepted},
		{"rate", "ERROR: (gcloud.projects.create) HttpError accessing <...>: response: <{'status': 429}>, content <{\"error\": {\"code\": 429, \"status\": \"RESOURCE_EXHAUSTED\"}}>", apperr.RateLimited},
		{"no account", "ERROR: (gcloud.auth.print-access-token) You do not currently have an active account selected.", apperr.NotLoggedIn},
		{"expired", "ERROR: (gcloud.auth.print-access-token) There was a problem refreshing your current auth tokens: ('invalid_grant: Bad Request')", apperr.NotLoggedIn},
		{"other", "ERROR: (gcloud.services.enable) something else broke", apperr.GcloudFailed},
	}
	for _, c := range cases {
		if got := Classify(c.stderr, errors.New("exit status 1")).Code; got != c.want {
			t.Errorf("%s: code = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestExtractLoginURL(t *testing.T) {
	const want = "https://accounts.google.com/o/oauth2/auth?a=1&b=2&c=3"
	cases := map[string]string{
		"one line":  "Go to the following link:\n\n    https://accounts.google.com/o/oauth2/auth?a=1&b=2&c=3\n\nOnce finished, enter the code: ",
		"wrapped":   "Go to the following link:\n\n    https://accounts.google.com/o/oauth2/auth?a=1&b\n=2&c=3\n\nOnce finished, enter the code: ",
		"no blank":  "Go:\n    https://accounts.google.com/o/oauth2/auth?a=1&b=2&c=3\nOnce finished, enter the code: ",
		"crlf wrap": "Go:\r\n\r\n    https://accounts.google.com/o/oauth2/auth?a=1&b=2\r\n&c=3\r\n\r\nOnce finished",
	}
	for name, in := range cases {
		if got := ExtractLoginURL(in); got != want {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if got := ExtractLoginURL("Go to the following link:\n\n    https://accounts.google.com/o/oauth2/auth?a=1"); got != "" {
		t.Errorf("partial line returned %q, want empty until the line is complete", got)
	}
}
