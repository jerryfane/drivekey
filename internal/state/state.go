// Package state owns drivekey's private directory and its config file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Dir is drivekey's state directory.
type Dir struct{ Root string }

// Resolve picks the state directory: $DRIVEKEY_HOME, then $XDG_CONFIG_HOME/drivekey,
// then ~/.config/drivekey. getenv and home are injected for tests.
func Resolve(getenv func(string) string, home func() (string, error)) (Dir, error) {
	if v := getenv("DRIVEKEY_HOME"); v != "" {
		return Dir{Root: v}, nil
	}
	if v := getenv("XDG_CONFIG_HOME"); v != "" {
		return Dir{Root: filepath.Join(v, "drivekey")}, nil
	}
	h, err := home()
	if err != nil {
		return Dir{}, fmt.Errorf("find home directory: %w", err)
	}
	return Dir{Root: filepath.Join(h, ".config", "drivekey")}, nil
}

// Default resolves the state directory from the real environment.
func Default() (Dir, error) { return Resolve(os.Getenv, os.UserHomeDir) }

// markerFile marks a directory as drivekey's own. drivekey refuses to use, and never
// deletes from, a non-empty directory without it, so a broad $DRIVEKEY_HOME (say, $HOME)
// is safe. The marker must hold exactly markerContent, so an unrelated file that happens
// to be named .drivekey does not count.
const (
	markerFile    = ".drivekey"
	markerContent = "drivekey state directory v1\n"
)

// Claim makes Root a drivekey state directory: it creates Root (0700) and the marker if Root
// is new or empty. It fails if Root already holds files but is not a drivekey state directory.
func (d Dir) Claim() error {
	if err := os.MkdirAll(d.Root, 0o700); err != nil {
		return err
	}
	if d.Owned() {
		return os.Chmod(d.Root, 0o700)
	}
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty and is not a drivekey state directory; set DRIVEKEY_HOME to an empty or new directory", d.Root)
	}
	if err := os.Chmod(d.Root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.Root, markerFile), []byte(markerContent), 0o600)
}

// Ensure claims Root and creates the subdirectories login needs, all with owner-only permissions.
func (d Dir) Ensure() error {
	if err := d.Claim(); err != nil {
		return err
	}
	for _, p := range []string{d.GcloudConfig(), d.LoginDir()} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Owned reports whether Root is a drivekey state directory: its marker is a regular file
// holding markerContent.
func (d Dir) Owned() bool {
	p := filepath.Join(d.Root, markerFile)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(markerContent)) {
		return false
	}
	b, err := os.ReadFile(p)
	return err == nil && string(b) == markerContent
}

// GcloudConfig is the private CLOUDSDK_CONFIG directory.
func (d Dir) GcloudConfig() string { return filepath.Join(d.Root, "gcloud") }

// LoginDir holds the state of a pending login.
func (d Dir) LoginDir() string { return filepath.Join(d.Root, "login") }

// TokenCache is the cached short-lived access token.
func (d Dir) TokenCache() string { return filepath.Join(d.Root, "token.json") }

// ChangesFile stores change-feed tokens.
func (d Dir) ChangesFile() string { return filepath.Join(d.Root, "changes.json") }

func (d Dir) configFile() string { return filepath.Join(d.Root, "config.json") }

// Clear deletes the state drivekey keeps: the login, cached token, change feeds and config.
// It deletes only those entries, never anything else in Root, and only if Root is a drivekey
// state directory. The marker and the lock file stay, so Clear can run while the lock is held
// and commands waiting on the lock keep locking the same file.
func (d Dir) Clear() error {
	if !d.Owned() {
		return nil
	}
	for _, p := range []string{d.GcloudConfig(), d.LoginDir(), d.TokenCache(), d.ChangesFile(), d.configFile()} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// Config is drivekey's persistent configuration.
type Config struct {
	Account string `json:"account,omitempty"`
	Project string `json:"project,omitempty"`
	// SetupComplete is true once the project exists and its APIs answer.
	SetupComplete bool `json:"setup_complete"`
}

// LoadConfig reads config.json; a missing file yields an empty Config.
func (d Dir) LoadConfig() (Config, error) {
	var c Config
	err := ReadJSON(d.configFile(), &c)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	return c, err
}

// SaveConfig writes config.json atomically.
func (d Dir) SaveConfig(c Config) error { return WriteJSON(d.configFile(), c) }

// ReadJSON decodes a JSON file into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// WriteJSON writes v to path atomically with mode 0600.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'))
}

// WriteFileAtomic writes data to a temp file in the same directory and renames it into place.
func WriteFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
