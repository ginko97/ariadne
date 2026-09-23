// Package config decides where ariadne keeps its data.
//
// Everything used to be relative to the working directory — runs/, workspace/,
// MEMORY.md, and a .env found by walking up to a go.mod. That works in a
// checkout and nowhere else: an installed binary run from two folders had two
// separate histories and never found its key. So there is one home directory,
// and everything lives under it.
package config

import (
	"errors"
	"path/filepath"
)

// HomeEnv overrides the home directory.
const HomeEnv = "ARIADNE_HOME"

// Paths is where one process reads and writes.
type Paths struct {
	Home      string // the data directory itself
	Runs      string // checkpoints and traces, one directory per run
	Workspace string // the default directory file tools are confined to
	Memory    string // MEMORY.md
	Env       string // config.env: keys and settings written by `ariadne setup`
	// Dev is true when Home is an ariadne checkout rather than a data
	// directory, which is what keeps a developer's existing runs/ and
	// workspace/ where they were.
	Dev bool
}

// Resolve picks the home directory, in order:
//
//  1. ARIADNE_HOME, when set — the operator's explicit choice.
//  2. repo, when a development build runs inside an ariadne checkout — the
//     layout this project has always had, so a developer's history does not
//     move. cmd/ariadne passes "" for a released binary (checkoutHome), whose
//     data must not depend on the folder it was started from.
//  3. userConfigDir()/ariadne — %AppData%\ariadne on Windows,
//     ~/Library/Application Support/ariadne on macOS, ~/.config/ariadne
//     elsewhere. Outside any workspace, so the keys in config.env are not
//     one ".." away from what file tools and exec work on.
//
// The functions are parameters so tests do not depend on the machine.
func Resolve(getenv func(string) string, repo string, userConfigDir func() (string, error)) (Paths, error) {
	var home string
	dev := false
	switch {
	case getenv(HomeEnv) != "":
		home = getenv(HomeEnv)
	case repo != "":
		home, dev = repo, true
	default:
		base, err := userConfigDir()
		if err != nil {
			return Paths{}, errors.Join(errors.New("config: no user config directory; set "+HomeEnv), err)
		}
		home = filepath.Join(base, "ariadne")
	}

	home, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Home:      home,
		Runs:      filepath.Join(home, "runs"),
		Workspace: filepath.Join(home, "workspace"),
		Memory:    filepath.Join(home, "MEMORY.md"),
		Env:       filepath.Join(home, "config.env"),
		Dev:       dev,
	}, nil
}
