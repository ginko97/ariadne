package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A task that works on files gets a folder of its own: a fresh temporary
// directory holding its fixtures, which the agent's file tools are confined
// to, and which is checked after the run and then removed. Tasks never share
// a folder, so one task's writes cannot help or break the next.
//
// Fixtures come from the task itself (Files, inline text) or from a folder
// next to the task file (FilesFrom, for binary documents). Both are held to
// relative paths inside that place. A task file is something a person may be
// handed by someone else, and every fixture is sent to the model being
// tested: a task naming "../../.ssh" would otherwise copy a person's keys into
// the workspace and hand them to a provider.

// prepareWorkspace makes t's folder and returns it with each fixture's
// original bytes, for the Unchanged check.
func prepareWorkspace(t Task) (string, map[string][]byte, error) {
	ws, err := os.MkdirTemp("", "ariadne-eval-*")
	if err != nil {
		return "", nil, err
	}
	fixtures := map[string][]byte{}
	fail := func(err error) (string, map[string][]byte, error) {
		_ = os.RemoveAll(ws)
		return "", nil, err
	}

	root, err := os.OpenRoot(ws)
	if err != nil {
		return fail(err)
	}
	defer root.Close()

	put := func(name string, data []byte) error {
		if err := safeRel(name); err != nil {
			return err
		}
		name = filepath.FromSlash(name)
		if dir := filepath.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := root.WriteFile(name, data, 0o644); err != nil {
			return err
		}
		fixtures[filepath.ToSlash(name)] = data
		return nil
	}

	if t.FilesFrom != "" {
		if err := safeRel(t.FilesFrom); err != nil {
			return fail(fmt.Errorf("files_from: %w", err))
		}
		src, err := os.OpenRoot(filepath.Join(t.dir, filepath.FromSlash(t.FilesFrom)))
		if err != nil {
			return fail(fmt.Errorf("files_from: %w", err))
		}
		err = fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			data, err := src.ReadFile(filepath.FromSlash(p))
			if err != nil {
				return err
			}
			return put(p, data)
		})
		src.Close()
		if err != nil {
			return fail(fmt.Errorf("files_from: %w", err))
		}
	}
	// Inline files after the folder, so a task can override one fixture.
	names := make([]string, 0, len(t.Files))
	for n := range t.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := put(n, []byte(t.Files[n])); err != nil {
			return fail(fmt.Errorf("files[%q]: %w", n, err))
		}
	}
	return ws, fixtures, nil
}

// checkFiles reports the first way the folder is not what t expects, or "".
func checkFiles(t Task, ws string, fixtures map[string][]byte) string {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return "cannot check files: " + err.Error()
	}
	defer root.Close()
	read := func(name string) ([]byte, error) { return root.ReadFile(filepath.FromSlash(name)) }

	for _, name := range sortedKeys(t.ExpectFile) {
		got, err := read(name)
		if err != nil {
			return fmt.Sprintf("%s: %v", name, missing(err))
		}
		if normEOL(got) != normEOL([]byte(t.ExpectFile[name])) {
			return fmt.Sprintf("%s is %q, want %q", name, excerpt(normEOL(got)), excerpt(normEOL([]byte(t.ExpectFile[name]))))
		}
	}
	for _, name := range sortedKeys(t.FileContains) {
		got, err := read(name)
		if err != nil {
			return fmt.Sprintf("%s: %v", name, missing(err))
		}
		if !strings.Contains(normEOL(got), normEOL([]byte(t.FileContains[name]))) {
			return fmt.Sprintf("%s does not contain %q", name, t.FileContains[name])
		}
	}
	for _, name := range t.Unchanged {
		want, ok := fixtures[filepath.ToSlash(name)]
		if !ok {
			return fmt.Sprintf("unchanged names %s, which is not a fixture", name)
		}
		got, err := read(name)
		if err != nil {
			return fmt.Sprintf("%s: %v", name, missing(err))
		}
		if !bytes.Equal(got, want) {
			return fmt.Sprintf("%s was changed", name)
		}
	}
	for _, name := range t.Absent {
		if _, err := read(name); err == nil {
			return fmt.Sprintf("%s exists and should not", name)
		}
	}
	return ""
}

// safeRel refuses a path that is absolute, has a volume or UNC prefix, or
// climbs out with "..": fixtures stay inside the place they are given.
func safeRel(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	slash := filepath.ToSlash(p)
	if filepath.IsAbs(p) || strings.HasPrefix(slash, "/") || filepath.VolumeName(p) != "" || strings.Contains(p, ":") {
		return fmt.Errorf("%q must be a relative path", p)
	}
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return fmt.Errorf("%q must stay inside its folder", p)
		}
	}
	return nil
}

func normEOL(b []byte) string {
	return strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

func missing(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "does not exist"
	}
	return err.Error()
}

func excerpt(s string) string {
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
