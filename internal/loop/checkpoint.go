package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const checkpointSchemaVersion = 1

// Checkpoint is the envelope written to disk. The version describes the file
// format, not the run — which is why it lives here and not on State.
type Checkpoint struct {
	SchemaVersion int       `json:"schema_version"`
	WrittenAt     time.Time `json:"written_at"`
	State         *State    `json:"state"`
}

// Store persists checkpoints under a directory, one subdirectory per run.
type Store struct{ Dir string } // e.g. "runs"

var ErrNoCheckpoint = errors.New("loop: no checkpoint for that run")

// runIDPattern guards against path traversal. RunID reaches Load straight from
// the command line — `ariadne resume ../../../etc` would otherwise escape the
// store via filepath.Join.
var runIDPattern = regexp.MustCompile(`^run_[0-9A-Za-z_-]+$`)

// ValidRunID reports whether id is safe and well-formed as a run identifier.
func ValidRunID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	return runIDPattern.MatchString(id)
}

// Save writes st atomically to <Dir>/<RunID>/checkpoint.json.
//
// Temp file, Sync, rename. Rename is atomic over an existing file on both
// Windows and Unix, so a reader never sees a partial write. The Sync is the
// step that is easy to skip and expensive to skip: rename orders the directory
// entry, not the bytes, so without it a power cut can leave a perfectly-renamed
// empty file — which looks valid, and is worse than no checkpoint at all.
func (s *Store) Save(st *State) error {
	if !ValidRunID(st.RunID) {
		return fmt.Errorf("loop: refusing to save run id %q", st.RunID)
	}

	dir := filepath.Join(s.Dir, st.RunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("loop: checkpoint dir: %w", err)
	}

	data, err := json.MarshalIndent(Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		WrittenAt:     time.Now().UTC(),
		State:         st,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("loop: encode checkpoint: %w", err)
	}

	final := filepath.Join(dir, "checkpoint.json")
	tmp := final + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("loop: create checkpoint: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("loop: write checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("loop: sync checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("loop: close checkpoint: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("loop: rename checkpoint: %w", err)
	}
	return nil
}

// Load reads the checkpoint for runID.
func (s *Store) Load(runID string) (*State, error) {
	if !ValidRunID(runID) {
		return nil, fmt.Errorf("loop: refusing to load run id %q", runID)
	}

	data, err := os.ReadFile(filepath.Join(s.Dir, runID, "checkpoint.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoCheckpoint, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("loop: read checkpoint: %w", err)
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("loop: decode checkpoint %s: %w", runID, err)
	}
	// A newer binary wrote this. Refuse rather than silently misreading fields.
	if cp.SchemaVersion > checkpointSchemaVersion {
		return nil, fmt.Errorf("loop: checkpoint %s is schema v%d, this binary understands v%d",
			runID, cp.SchemaVersion, checkpointSchemaVersion)
	}
	if cp.State == nil {
		return nil, fmt.Errorf("loop: checkpoint %s contains no state", runID)
	}
	return cp.State, nil
}
