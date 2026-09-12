package eval

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndLoadHistory(t *testing.T) {
	dir := t.TempDir()

	older := NewScorecard("vendor/model-a", "aaa1111", []Result{{TaskID: "t1", Pass: true}})
	older.When = time.Now().Add(-time.Hour).UTC()
	newer := NewScorecard("vendor/model-a", "bbb2222", []Result{{TaskID: "t1", Pass: false}})

	for _, sc := range []Scorecard{older, newer} {
		path, err := sc.Save(dir)
		if err != nil {
			t.Fatal(err)
		}
		// Model ids carry slashes; a filename cannot.
		if strings.Contains(filepath.Base(path), "/") {
			t.Errorf("unsafe filename: %s", path)
		}
	}

	history, err := LoadHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d scorecards, want 2", len(history))
	}
	if history[0].Commit != "aaa1111" {
		t.Errorf("history is not oldest-first: %s then %s", history[0].Commit, history[1].Commit)
	}
}

func TestLoadHistoryMissingDirIsEmpty(t *testing.T) {
	history, err := LoadHistory(filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatalf("no history yet should not be an error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("got %d scorecards", len(history))
	}
}

// Comparing against another model would measure the models, not the change.
func TestPreviousIgnoresOtherModels(t *testing.T) {
	history := []Scorecard{
		{Model: "a", Commit: "1"},
		{Model: "b", Commit: "2"},
		{Model: "a", Commit: "3"},
	}
	got, ok := Previous(history, "a")
	if !ok || got.Commit != "3" {
		t.Fatalf("got %+v, want the latest scorecard for a", got)
	}
	if _, ok := Previous(history, "never-run"); ok {
		t.Error("reported a previous scorecard for a model with no history")
	}
}

// A pass rate drop says something changed; a regression list says what.
func TestRegressionsNamesTasks(t *testing.T) {
	before := Scorecard{Results: []Result{
		{TaskID: "t1", Pass: true},
		{TaskID: "t2", Pass: true},
		{TaskID: "t3", Pass: false},
	}}
	after := Scorecard{Results: []Result{
		{TaskID: "t1", Pass: true},
		{TaskID: "t2", Pass: false}, // regression
		{TaskID: "t3", Pass: false}, // already failing: not a regression
	}}

	got := Regressions(before, after)
	if len(got) != 1 || got[0] != "t2" {
		t.Fatalf("got %v, want [t2]", got)
	}
}

// Two sweeps at the same commit must both survive. Collapsing them would hide
// run-to-run variance, which is the thing the repeats exist to measure.
func TestSaveKeepsRepeatsAtSameCommit(t *testing.T) {
	dir := t.TempDir()

	first := NewScorecard("vendor/model", "same123", []Result{{TaskID: "t1", Pass: true}})
	first.When = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	second := NewScorecard("vendor/model", "same123", []Result{{TaskID: "t1", Pass: false}})
	second.When = time.Date(2026, 9, 12, 10, 5, 0, 0, time.UTC)

	p1, err := first.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := second.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("both sweeps wrote to %s — the second overwrote the first", p1)
	}

	history, err := LoadHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d scorecards, want 2", len(history))
	}
	if history[0].Passed != 1 || history[1].Passed != 0 {
		t.Errorf("history is not oldest-first: %+v", history)
	}
}

// Pass/fail alone missed a real defect: reintroducing a truncation bug in the
// calculator left the pass rate at 34/34, because the model got a wrong number,
// re-checked it a second way, and answered correctly from its own arithmetic.
// The only evidence was an extra tool call.
func TestStepRegressionsFindsSilentExtraWork(t *testing.T) {
	before := Scorecard{Results: []Result{
		{TaskID: "chain", Pass: true, Steps: 4},
		{TaskID: "simple", Pass: true, Steps: 2},
		{TaskID: "other", Pass: true, Steps: 3},
	}}
	after := Scorecard{Results: []Result{
		{TaskID: "chain", Pass: true, Steps: 5},  // same answer, more work
		{TaskID: "simple", Pass: true, Steps: 2}, // unchanged
		{TaskID: "other", Pass: true, Steps: 2},  // fewer: not a regression
	}}

	got := StepRegressions(before, after)
	if len(got) != 1 || got[0] != "chain 4->5" {
		t.Fatalf("got %v, want [chain 4->5]", got)
	}
}

// A task with no history cannot have regressed.
func TestStepRegressionsIgnoresNewTasks(t *testing.T) {
	before := Scorecard{Results: []Result{{TaskID: "old", Pass: true, Steps: 2}}}
	after := Scorecard{Results: []Result{
		{TaskID: "old", Pass: true, Steps: 2},
		{TaskID: "brand-new", Pass: true, Steps: 9},
	}}
	if got := StepRegressions(before, after); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
