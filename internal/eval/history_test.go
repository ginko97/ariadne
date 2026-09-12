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
