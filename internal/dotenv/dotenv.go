package dotenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load walks up to the repo root (the directory holding go.mod) and reads
// KEY=value pairs from .env into the environment. Existing variables win.
// A missing .env or a missing go.mod is not an error.
//
// No testing import on purpose — that would link the testing package into any
// binary importing this.
func Load() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	origDir := dir
	found := false
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			found = true
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if !found {
		dir = origDir
	}

	f, err := os.Open(filepath.Join(dir, ".env"))
	if err != nil {
		return nil // no .env is fine
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
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
