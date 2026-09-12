package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Save writes a scorecard to <dir>/<commit>_<model>.json.
//
// Committed, unlike runs/: a pass rate is only meaningful next to the pass rates
// before it, and that history has to travel with the code that produced it.
func (sc Scorecard) Save(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("eval: history dir: %w", err)
	}

	name := fmt.Sprintf("%s_%s.json", orUnknown(sc.Commit), slug(sc.Model))
	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("eval: encode scorecard: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("eval: write scorecard: %w", err)
	}
	return path, nil
}

// LoadHistory reads every scorecard in dir, oldest first.
func LoadHistory(dir string) ([]Scorecard, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // no history yet is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("eval: read history: %w", err)
	}

	var out []Scorecard
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("eval: read %s: %w", e.Name(), err)
		}
		var sc Scorecard
		if err := json.Unmarshal(data, &sc); err != nil {
			return nil, fmt.Errorf("eval: decode %s: %w", e.Name(), err)
		}
		out = append(out, sc)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].When.Before(out[j].When) })
	return out, nil
}

// Previous returns the most recent earlier scorecard for the same model, which
// is what a new one has to be compared against. Comparing across models would
// measure the models, not the change.
func Previous(history []Scorecard, model string) (Scorecard, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Model == model {
			return history[i], true
		}
	}
	return Scorecard{}, false
}

// Regressions lists tasks that passed in before and fail in sc.
//
// A drop in pass rate says something changed; this says what, which is the
// difference between a number to worry about and a trace to read.
func Regressions(before, sc Scorecard) []string {
	passed := map[string]bool{}
	for _, r := range before.Results {
		if r.Pass {
			passed[r.TaskID] = true
		}
	}

	var out []string
	for _, r := range sc.Results {
		if !r.Pass && passed[r.TaskID] {
			out = append(out, r.TaskID)
		}
	}
	sort.Strings(out)
	return out
}

// slug makes a model id safe as a filename: ids carry slashes and colons.
func slug(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
