package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePrecedence(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"DRIVEKEY_HOME": "/x", "XDG_CONFIG_HOME": "/xdg"}, "/x"},
		{map[string]string{"XDG_CONFIG_HOME": "/xdg"}, "/xdg/drivekey"},
		{map[string]string{}, "/home/u/.config/drivekey"},
	}
	for _, c := range cases {
		d, err := Resolve(env(c.vars), home)
		if err != nil || d.Root != c.want {
			t.Errorf("%v: got %q %v, want %q", c.vars, d.Root, err, c.want)
		}
	}
	if _, err := Resolve(env(nil), func() (string, error) { return "", errors.New("no home") }); err == nil {
		t.Error("missing home must fail")
	}
}

func TestForeignDirectoryIsNeverUsedOrDeleted(t *testing.T) {
	// A non-empty directory without the marker, e.g. DRIVEKEY_HOME=$HOME with its own gcloud dir.
	root := t.TempDir()
	foreign := filepath.Join(root, "gcloud", "foreign.txt")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := Dir{Root: root}
	if err := d.Ensure(); err == nil {
		t.Fatal("Ensure must refuse a non-empty directory without the marker")
	}
	if err := d.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := d.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file deleted: %v", err)
	}
}

func TestClearAndPurgeOwnDirectory(t *testing.T) {
	d := Dir{Root: filepath.Join(t.TempDir(), "dk")}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveConfig(Config{Account: "a"}); err != nil {
		t.Fatal(err)
	}
	l, err := d.Lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Clear(); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.LoadConfig(); c.Account != "" {
		t.Fatal("config survived Clear")
	}
	l.Unlock()
	if err := d.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Root); !os.IsNotExist(err) {
		t.Fatalf("root kept: %v", err)
	}
}

func TestLockExcludesOtherHolders(t *testing.T) {
	d := Dir{Root: filepath.Join(t.TempDir(), "dk")}
	ctx := context.Background()
	l1, err := d.Lock(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Lock(ctx, 200*time.Millisecond); err == nil {
		t.Fatal("second Lock succeeded while the first is held")
	}
	l1.Unlock()
	l2, err := d.Lock(ctx, time.Second)
	if err != nil {
		t.Fatalf("Lock after Unlock: %v", err)
	}
	l2.Unlock()
}

func TestEnsureAndConfigArePrivate(t *testing.T) {
	d := Dir{Root: filepath.Join(t.TempDir(), "dk")}
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Root, d.GcloudConfig(), d.LoginDir()} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v err %v", p, st.Mode().Perm(), err)
		}
	}
	if c, err := d.LoadConfig(); err != nil || c != (Config{}) {
		t.Fatalf("missing config: %+v %v", c, err)
	}
	want := Config{Account: "a@b.c", Project: "p", SetupComplete: true}
	if err := d.SaveConfig(want); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(d.Root, "config.json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", st.Mode().Perm())
	}
	if got, err := d.LoadConfig(); err != nil || got != want {
		t.Fatalf("round trip %+v %v", got, err)
	}
}
