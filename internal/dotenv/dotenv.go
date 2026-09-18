package dotenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// module is the module path a go.mod must declare for its directory to count
// as an ariadne checkout.
const module = "github.com/ginko97/ariadne"

// Load reads .env from the root of an ariadne checkout, if the working
// directory is inside one. Existing variables win. No checkout, or no .env in
// it, is not an error.
//
// Only an ariadne checkout. This used to fall back to the working directory's
// .env when no go.mod was found, for the benefit of an installed binary — which
// meant an installed binary run inside somebody's project loaded that
// project's secrets into its own environment. Installed binaries now read
// config.env from their data directory instead (internal/config), and a
// stranger's .env is left alone.
//
// No testing import on purpose — that would link the testing package into any
// binary importing this.
func Load() error {
	dir, ok := Repo()
	if !ok {
		return nil
	}
	return LoadFile(filepath.Join(dir, ".env"))
}

// Repo returns the root of the ariadne checkout containing the working
// directory: the nearest directory upward whose go.mod declares this module.
func Repo() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if isAriadneModule(filepath.Join(dir, "go.mod")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isAriadneModule(goMod string) bool {
	f, err := os.Open(goMod)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.TrimSpace(rest) == module
		}
	}
	return false
}

// LoadFile reads KEY=value pairs from path into the environment. Existing
// variables win, so the process environment always overrides a file. A
// missing file is not an error.
func LoadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil // no file is fine
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
