package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func userDir(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// An installed binary keeps everything in one place, whatever folder it is run
// from — the point of the package.
func TestResolveUsesTheUserConfigDirectory(t *testing.T) {
	base := t.TempDir()
	p, err := Resolve(env(nil), "", userDir(base))
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "ariadne")
	want := Paths{
		Home:      home,
		Runs:      filepath.Join(home, "runs"),
		Workspace: filepath.Join(home, "workspace"),
		Memory:    filepath.Join(home, "MEMORY.md"),
		Env:       filepath.Join(home, "config.env"),
	}
	if p != want {
		t.Errorf("got  %+v\nwant %+v", p, want)
	}
}

// Inside a checkout, home is the repo: runs/, workspace/ and MEMORY.md stay
// exactly where a developer already has them.
func TestResolveKeepsACheckoutsLayout(t *testing.T) {
	repo := t.TempDir()
	p, err := Resolve(env(nil), repo, userDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Home != repo || !p.Dev || p.Runs != filepath.Join(repo, "runs") {
		t.Errorf("checkout layout not kept: %+v", p)
	}
}

// ARIADNE_HOME beats both, and a relative one is made absolute so the paths
// recorded on checkpoints do not depend on where the next command runs.
func TestResolveHonoursAriadneHome(t *testing.T) {
	chosen := t.TempDir()
	p, err := Resolve(env(map[string]string{HomeEnv: chosen}), t.TempDir(), userDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Home != chosen || p.Dev {
		t.Errorf("ARIADNE_HOME not honoured: %+v", p)
	}

	t.Chdir(t.TempDir())
	p, err = Resolve(env(map[string]string{HomeEnv: "rel"}), "", userDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Home) {
		t.Errorf("relative ARIADNE_HOME left relative: %q", p.Home)
	}
}

func TestResolveSaysWhatToDoWithNoConfigDirectory(t *testing.T) {
	_, err := Resolve(env(nil), "", func() (string, error) { return "", errors.New("$HOME is not defined") })
	if err == nil {
		t.Fatal("no error without a config directory")
	}
	if want := HomeEnv; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
}
