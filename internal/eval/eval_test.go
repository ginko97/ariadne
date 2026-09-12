package eval

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// stateWithCall builds a finished run that used the named tools.
func stateWithCall(steps int, cost float64, toolNames ...string) *loop.State {
	s := loop.NewState("run_eval", "task")
	s.Steps, s.Cost = steps, cost
	for i, name := range toolNames {
		s.Messages = append(s.Messages,
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Type: llm.BlockToolUse,
				ID:   "call_" + name,
				Name: name,
				Args: json.RawMessage(`{}`),
			}}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{
				Type:    llm.BlockToolResult,
				CallID:  "call_" + name,
				Content: "ok",
			}}},
		)
		_ = i
	}
	return s
}

var calcTask = Task{
	ID:       "pct-01",
	Prompt:   "What is 15% of 240?",
	Expect:   "36",
	MustCall: []string{"calc"},
}

func TestScorePasses(t *testing.T) {
	got := Score(calcTask, stateWithCall(2, 0.0001, "calc"), "15% of 240 is 36.", nil)
	if !got.Pass {
		t.Fatalf("want pass, got %+v", got)
	}
	if got.Steps != 2 || got.Cost != 0.0001 || got.RunID != "run_eval" {
		t.Errorf("result did not carry run facts: %+v", got)
	}
}

// The finding that made MustCall exist: mistral-nemo answered "36" correctly in
// one step, having computed it itself and called nothing. Answer-only scoring
// would have marked the cheapest model as the best one.
func TestScoreFailsWhenToolNeverRan(t *testing.T) {
	got := Score(calcTask, stateWithCall(1, 0.00003), "36", nil)
	if got.Pass {
		t.Fatal("scored a pass despite the tool never running")
	}
	if !strings.Contains(got.Reason, "calc") {
		t.Errorf("reason should name the missing tool: %q", got.Reason)
	}
}

// The other finding: deepseek and ling answer in markdown. Formatting is not a
// wrong answer.
func TestScoreAcceptsMarkdownFormatting(t *testing.T) {
	for _, answer := range []string{
		"15% of 240 is **36**.",
		"The answer is `36`",
		"  15% OF 240   IS 36  ",
		"_36_",
	} {
		if got := Score(calcTask, stateWithCall(2, 0, "calc"), answer, nil); !got.Pass {
			t.Errorf("%q scored as a failure: %s", answer, got.Reason)
		}
	}
}

func TestScoreFailsOnWrongAnswer(t *testing.T) {
	got := Score(calcTask, stateWithCall(2, 0, "calc"), "the answer is 42", nil)
	if got.Pass {
		t.Fatal("scored a pass on a wrong answer")
	}
	if !strings.Contains(got.Reason, "36") {
		t.Errorf("reason should say what was expected: %q", got.Reason)
	}
}

// Single-digit expectations like "1" must not match inside "10", "15", or "1/3".
func TestScoreRejectsSubstringFalsePass(t *testing.T) {
	modTask := Task{ID: "mod-01", Expect: "1", MustCall: []string{"calc"}}

	// Model echoed prompt ("10") and gave wrong answer ("2"):
	echoWrong := Score(modTask, stateWithCall(2, 0, "calc"),
		"When 10 is divided by 3, the remainder is 2.", nil)
	if echoWrong.Pass {
		t.Fatal("expected failure: '1' matched inside '10'")
	}

	// Model echoed fraction ("1/3"):
	fracWrong := Score(modTask, stateWithCall(2, 0, "calc"),
		"Using the calc tool, 1/3 multiplied by 3 gives 0.999.", nil)
	if fracWrong.Pass {
		t.Fatal("expected failure: '1' matched inside '1/3'")
	}

	// Model said decimal ("1.5"):
	decWrong := Score(modTask, stateWithCall(2, 0, "calc"),
		"The answer is 1.5", nil)
	if decWrong.Pass {
		t.Fatal("expected failure: '1' matched inside '1.5'")
	}

	// Model answered with negative number ("-1"):
	negWrong := Score(modTask, stateWithCall(2, 0, "calc"),
		"The remainder is -1.", nil)
	if negWrong.Pass {
		t.Fatal("expected failure: '1' matched inside '-1'")
	}

	// Positive expectation must reject negative answer:
	posTask := Task{ID: "prec-02", Expect: "25", MustCall: []string{"calc"}}
	negAnswer := Score(posTask, stateWithCall(2, 0, "calc"),
		"The answer is -25.", nil)
	if negAnswer.Pass {
		t.Fatal("expected failure: '25' matched inside '-25'")
	}

	// Negative expectation must accept negative answer and reject positive answer:
	negTask := Task{ID: "neg-01", Expect: "-25", MustCall: []string{"calc"}}
	negCorrect := Score(negTask, stateWithCall(2, 0, "calc"),
		"The result is -25.", nil)
	if !negCorrect.Pass {
		t.Fatalf("expected pass for negative answer: %s", negCorrect.Reason)
	}
	posWrong := Score(negTask, stateWithCall(2, 0, "calc"),
		"The result is 25.", nil)
	if posWrong.Pass {
		t.Fatal("expected failure: '+25' matched '-25'")
	}

	// Model answered correctly:
	correct := Score(modTask, stateWithCall(2, 0, "calc"),
		"When 10 is divided by 3, the remainder is 1.", nil)
	if !correct.Pass {
		t.Fatalf("expected pass, got failure: %s", correct.Reason)
	}
}

// A failed run is a failed task, but the reason has to survive — the failure
// taxonomy is built by reading these.
func TestScoreCarriesRunError(t *testing.T) {
	s := stateWithCall(5, 0.002, "calc")
	got := Score(calcTask, s, "", loop.ErrStepLimit)
	if got.Pass {
		t.Fatal("scored a pass on a failed run")
	}
	if !strings.Contains(got.Reason, "step limit") {
		t.Errorf("reason lost the cause: %q", got.Reason)
	}
	if got.Steps != 5 || got.Cost != 0.002 {
		t.Errorf("a failed run still costs money and steps: %+v", got)
	}
}

// A task with no MustCall scores on the answer alone.
func TestScoreWithoutMustCall(t *testing.T) {
	t2 := Task{ID: "free-01", Expect: "paris"}
	if got := Score(t2, stateWithCall(1, 0), "The capital is Paris.", nil); !got.Pass {
		t.Errorf("want pass, got %+v", got)
	}
}

func TestNewScorecard(t *testing.T) {
	sc := NewScorecard("some/model", "abc1234", []Result{
		{TaskID: "a", Pass: true, Cost: 0.001},
		{TaskID: "b", Pass: false, Cost: 0.002, Reason: "wrong"},
		{TaskID: "c", Pass: true, Cost: 0.003},
	})

	if sc.Passed != 2 || sc.Total != 3 {
		t.Errorf("passed/total = %d/%d, want 2/3", sc.Passed, sc.Total)
	}
	if sc.PassRate < 0.66 || sc.PassRate > 0.67 {
		t.Errorf("PassRate = %v", sc.PassRate)
	}
	// Cost comes from the runs, so it is what was charged, not an estimate.
	if sc.TotalCost < 0.0059 || sc.TotalCost > 0.0061 {
		t.Errorf("TotalCost = %v, want 0.006", sc.TotalCost)
	}
}

func TestLoadTasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	body := `[{"id":"a","prompt":"p","expect":"x","must_call":["calc"],"max_steps":5}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tasks, err := LoadTasks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "a" || tasks[0].MustCall[0] != "calc" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

// Duplicate ids would silently overwrite each other when comparing scorecards
// task by task across models.
func TestLoadTasksRejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte(`[{"id":"a"},{"id":"a"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTasks(path); err == nil {
		t.Fatal("accepted duplicate ids")
	}
}

func TestLoadTasksRejectsMissingID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte(`[{"prompt":"p"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTasks(path); err == nil {
		t.Fatal("accepted a task with no id")
	}
}

func TestLoadTasksMissingFile(t *testing.T) {
	if _, err := LoadTasks(filepath.Join(t.TempDir(), "nope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want a not-exist error", err)
	}
}
