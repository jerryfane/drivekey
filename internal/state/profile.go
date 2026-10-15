package state

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DirectProfile selects the state directory's own gcloud login even when relay profiles exist.
const DirectProfile = "direct"

// Profile is a named Keyring relay route to one Google account. It holds identifiers only:
// the Google login lives on the Keyring host and the role token in the relay's own storage.
type Profile struct {
	// Relay is the local Keyring relay, e.g. http://127.0.0.1:7700.
	Relay string `json:"relay"`
	// Role is the Keyring role the relay authenticates as.
	Role string `json:"role"`
	// Account names the Keyring Google account; its services are google-<account>-drive,
	// google-<account>-upload and google-<account>-sheets.
	Account string `json:"account"`
	// QuotaProject, if set, is sent as X-Goog-User-Project. Keyring may override it.
	QuotaProject string `json:"quota_project,omitempty"`
	// ID changes every time the profile is added, so state recorded under an earlier
	// definition of the same name is never reused.
	ID string `json:"id"`
}

// DriveService, UploadService and SheetsService are the Keyring services of p's account.
func (p Profile) DriveService() string  { return "google-" + p.Account + "-drive" }
func (p Profile) UploadService() string { return "google-" + p.Account + "-upload" }
func (p Profile) SheetsService() string { return "google-" + p.Account + "-sheets" }

// Identity names the account, endpoint and service a profile's state belongs to.
func (p Profile) Identity() string {
	return "relay " + p.Relay + "/" + p.Role + "/" + p.DriveService()
}

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	rolePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`) // Keyring's role names
	projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
)

// ValidName reports whether s can name a profile or account. The names appear in URL paths
// and file names, so they are restricted to lower-case letters, digits, '-' and '_'.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// NewProfile validates the parts of a profile and returns it with a fresh ID.
func NewProfile(relay, role, account, quotaProject string) (Profile, error) {
	r, err := normalizeRelay(relay)
	if err != nil {
		return Profile{}, err
	}
	if !rolePattern.MatchString(role) {
		return Profile{}, fmt.Errorf("invalid role %q: use letters, digits, '.', '-' and '_'", role)
	}
	if !ValidName(account) {
		return Profile{}, fmt.Errorf("invalid account %q: use lower-case letters, digits, '-' and '_'", account)
	}
	if quotaProject != "" && !projectPattern.MatchString(quotaProject) {
		return Profile{}, fmt.Errorf("invalid quota project id %q", quotaProject)
	}
	return Profile{Relay: r, Role: role, Account: account, QuotaProject: quotaProject, ID: newSession()}, nil
}

// normalizeRelay accepts an http(s) origin with an optional path prefix. Plain http is only
// accepted for loopback addresses, where the local relay listens.
func normalizeRelay(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid relay URL %q: want e.g. http://127.0.0.1:7700", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("invalid relay URL %q: no credentials, query or fragment allowed", s)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("relay URL %q: plain http is only allowed on a loopback address", s)
		}
	default:
		return "", fmt.Errorf("invalid relay URL %q: want http or https", s)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Profiles maps profile names to profiles.
type Profiles map[string]Profile

func (d Dir) profilesFile() string { return filepath.Join(d.Root, "profiles.json") }

// ProfileDir holds the local state of one relay profile (its change feeds).
func (d Dir) ProfileDir(name string) string { return filepath.Join(d.Root, "relay", name) }

// ProfileChangesFile stores the change-feed positions of one relay profile.
func (d Dir) ProfileChangesFile(name string) string {
	return filepath.Join(d.ProfileDir(name), "changes.json")
}

// LoadProfiles reads profiles.json; a missing file yields no profiles.
func (d Dir) LoadProfiles() (Profiles, error) {
	p := Profiles{}
	err := ReadJSON(d.profilesFile(), &p)
	if errors.Is(err, os.ErrNotExist) {
		return Profiles{}, nil
	}
	if p == nil {
		p = Profiles{}
	}
	return p, err
}

// SaveProfiles writes profiles.json atomically.
func (d Dir) SaveProfiles(p Profiles) error { return WriteJSON(d.profilesFile(), p) }

// RemoveProfileState deletes a relay profile's local state, and only that.
func (d Dir) RemoveProfileState(name string) error {
	if !ValidName(name) || !d.Owned() {
		return nil
	}
	return os.RemoveAll(d.ProfileDir(name))
}

// EnsureProfileDir creates a relay profile's state directory with owner-only permissions.
func (d Dir) EnsureProfileDir(name string) error {
	if err := d.Claim(); err != nil {
		return err
	}
	for _, p := range []string{filepath.Dir(d.ProfileDir(name)), d.ProfileDir(name)} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}
