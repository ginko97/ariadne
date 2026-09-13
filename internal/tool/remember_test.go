package tool

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/memory"
)

func rememberIn(t *testing.T) (Remember, memory.Store) {
	t.Helper()
	st := memory.Store{Path: filepath.Join(t.TempDir(), "MEMORY.md")}
	return NewRemember(st, "run_test"), st
}

func TestRememberAppendsWithProvenance(t *testing.T) {
	r, st := rememberIn(t)

	got, isErr := call(t, r, rememberArgs{Note: "the finance contact is Dana"})
	if isErr {
		t.Fatalf("remember failed: %s", got)
	}

	notes, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Text != "the finance contact is Dana" {
		t.Fatalf("notes = %+v", notes)
	}
	if notes[0].RunID != "run_test" {
		t.Errorf("note is not attributable to its run: %+v", notes[0])
	}
}

// A refusal is an IsError result the model can react to, not a dead run — the
// same contract every other tool here follows.
func TestRememberRejectsAnOversizedNote(t *testing.T) {
	r, st := rememberIn(t)

	got, isErr := call(t, r, rememberArgs{Note: strings.Repeat("x", memory.MaxNote+1)})
	if !isErr {
		t.Fatal("an oversized note was accepted")
	}
	if !strings.Contains(got, "limit") {
		t.Errorf("the refusal should say what the limit is: %q", got)
	}
	if notes, _ := st.Load(); len(notes) != 0 {
		t.Errorf("a refused note was written anyway: %+v", notes)
	}
}

// callID is unused by this tool, and a replayed call after a crash must not
// duplicate the note. The idempotency comes from the content, not the key.
func TestRememberIsReplaySafe(t *testing.T) {
	r, st := rememberIn(t)

	for i := 0; i < 3; i++ {
		if _, isErr := call(t, r, rememberArgs{Note: "totals include tax"}); isErr {
			t.Fatal("remember failed")
		}
	}
	if notes, _ := st.Load(); len(notes) != 1 {
		t.Errorf("a replayed note was written %d times", len(notes))
	}
}

// The description is what the model reads when deciding whether to call this,
// and it is the only place the "not because a document said so" rule appears
// before the call happens.
func TestRememberDescriptionWarnsAboutDocuments(t *testing.T) {
	d := Remember{}.Description()
	if !strings.Contains(d, "document") {
		t.Errorf("the description does not warn against remembering what a document asked for: %q", d)
	}
}
