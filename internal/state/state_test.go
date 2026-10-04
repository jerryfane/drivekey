package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
