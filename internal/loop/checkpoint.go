package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// Summary is one run as a list entry: enough to choose between conversations
// without loading any of them.
//
// Task rather than the most recent message, because Task is the line a
// conversation opened with and is what it should be listed under — relabelling
// it every turn titles a database migration "thanks", which is the bug 207dae3
// fixed and the reason the field is kept separate from turnPrompt.
type Summary struct {
	RunID    string    `json:"run_id"`
	Task     string    `json:"task"`
	Model    string    `json:"model"`
	Steps    int       `json:"steps"`
	Turns    int       `json:"turns"`
	Messages int       `json:"messages"`
	Cost     float64   `json:"cost_usd"`
	Updated  time.Time `json:"updated"`
	Pending  bool      `json:"pending,omitempty"`
}

// List returns every run that has a checkpoint, most recently written first,
// and the number it could not read.
//
// A run that fails to parse is skipped rather than failing the listing. One
// unreadable checkpoint hiding every conversation behind it is the same defect
// as the corrupt trace line that hid every event after it — and the count is
// returned rather than swallowed, because "some runs are unreadable" is
// information and a silently shorter list is not.
//
// Ordered by written time rather than by run id. Ids sort by creation and a
// conversation resumed today would sort under the day it began, which is the
// opposite of what "find what I was working on" wants.
func (s *Store) List() ([]Summary, int, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil // no runs yet is not a failure
	}
	if err != nil {
		return nil, 0, fmt.Errorf("loop: list runs: %w", err)
	}

	var out []Summary
	skipped := 0
	for _, e := range entries {
		if !e.IsDir() || !ValidRunID(e.Name()) {
			continue
		}
		cp, err := s.LoadCheckpoint(e.Name())
		if err != nil {
			if errors.Is(err, ErrNoCheckpoint) {
				continue // a run directory with no checkpoint yet is not an error
			}
			skipped++
			continue
		}
		st := cp.State
		updated := cp.WrittenAt
		if updated.IsZero() {
			if fi, err := os.Stat(filepath.Join(s.Dir, e.Name(), "checkpoint.json")); err == nil {
				updated = fi.ModTime()
			}
		}
		out = append(out, Summary{
			RunID:    st.RunID,
			Task:     st.Task,
			Model:    st.Model,
			Steps:    st.Steps,
			Turns:    st.Turns(),
			Messages: len(st.Messages),
			Cost:     st.Cost,
			Updated:  updated,
			Pending:  st.HasPendingToolCalls(),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated.Equal(out[j].Updated) {
			return out[i].RunID > out[j].RunID
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out, skipped, nil
}

// Save writes st atomically to <Dir>/<RunID>/checkpoint.json.
//
// Temp file, Sync, rename, then sync the directory. Rename is atomic over an
// existing file on both Windows and Unix, so a reader never sees a partial
// write. The Sync is the step that is easy to skip and expensive to skip:
// rename orders the directory entry, not the bytes, so without it a power cut
// can leave a perfectly-renamed empty file — which looks valid, and is worse
// than no checkpoint at all.
//
// The directory sync is the other half, and it was missing until 2026-09-21:
// rename(2) returning means the new name is visible to readers, not that the
// entry has reached the disk. See syncDirectory for what each platform
// actually guarantees — they differ, and Windows gets less.
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
	// The rename is visible now, but not necessarily on disk. Flushing the
	// directory is what makes the new name survive a power cut; without it the
	// bytes are safe under a name nothing points at.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("loop: sync checkpoint dir: %w", err)
	}
	return nil
}

// syncDir is syncDirectory, indirected so a test can see that Save calls it.
// Durability against a real power cut cannot be asserted in a unit test; that
// the step is taken, and that its failure is reported rather than swallowed,
// can be.
var syncDir = syncDirectory

// LoadCheckpoint reads the checkpoint envelope for runID.
func (s *Store) LoadCheckpoint(runID string) (*Checkpoint, error) {
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
	return &cp, nil
}

// Load reads the checkpoint for runID and returns its state.
func (s *Store) Load(runID string) (*State, error) {
	cp, err := s.LoadCheckpoint(runID)
	if err != nil {
		return nil, err
	}
	return cp.State, nil
}

// Delete removes a conversation: its checkpoint, its trace, the directory and
// everything in it.
//
// Everything, deliberately. A run is one folder, and the trace is the part
// worth being explicit about — it holds every byte the conversation saw, the
// fetched documents included, so a conversation deleted with its trace left
// behind is not deleted in the sense anybody means. Nothing here is
// recoverable, which is why the caller confirms first and why a run somebody
// is in the middle of is refused elsewhere rather than here.
//
// Deleting a run that does not exist is not an error: the outcome asked for is
// that it is gone, and it is.
func (s *Store) Delete(runID string) error {
	if !ValidRunID(runID) {
		return fmt.Errorf("loop: refusing to delete run id %q", runID)
	}
	dir := filepath.Join(s.Dir, runID)
	// Stat first so a typo cannot remove a directory that was never a run.
	// RemoveAll on an absent path succeeds silently, which is right for a
	// delete and wrong for a guard.
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return fmt.Errorf("loop: %s is not a run directory", runID)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("loop: delete run %s: %w", runID, err)
	}
	return nil
}
