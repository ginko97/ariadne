// Package eval scores finished runs against a fixed task set.
//
// Scoring is deliberately a pure function of a completed loop.State: it makes
// no API calls, so the scorer can be built and tested before a single token is
// spent, and a saved run can be re-scored later when the rules change.
package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// Task is one thing the agent is asked to do, plus how to tell whether it did.
type Task struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`

	// Expect is matched as a substring of the answer after normalisation, not
	// byte-for-byte: models format the same fact differently and a formatting
	// difference is not a wrong answer.
	Expect string `json:"expect"`

	// MustCall names tools that have to actually have run.
	//
	// Without this a model that answers arithmetic from its own head scores as
	// a pass while never touching the tool — observed, not hypothetical: on
	// "what is 15% of 240", mistral-nemo answered correctly in one step and
	// called nothing.
	MustCall []string `json:"must_call,omitempty"`

	MaxSteps int `json:"max_steps,omitempty"`
}

// Result is one task's outcome.
type Result struct {
	TaskID string  `json:"task_id"`
	Pass   bool    `json:"pass"`
	Reason string  `json:"reason,omitempty"` // why it failed; empty on a pass
	Steps  int     `json:"steps"`
	Cost   float64 `json:"cost_usd"`
	Answer string  `json:"answer"`
	RunID  string  `json:"run_id"` // the trace to read when this fails
}

// Scorecard is one model's run over one task set, at one commit.
type Scorecard struct {
	Model     string    `json:"model"`
	Commit    string    `json:"commit"`
	When      time.Time `json:"when"`
	Results   []Result  `json:"results"`
	Passed    int       `json:"passed"`
	Total     int       `json:"total"`
	PassRate  float64   `json:"pass_rate"`
	TotalCost float64   `json:"total_cost_usd"`
}

// Score judges one finished run. It never inspects the model or the provider —
// only what the run actually did.
func Score(t Task, s *loop.State, answer string, runErr error) Result {
	r := Result{TaskID: t.ID, Answer: answer}
	if s != nil {
		r.Steps, r.Cost, r.RunID = s.Steps, s.Cost, s.RunID
	}

	// A run that failed is a failed task, but the reason matters: a step limit
	// is a different problem from a provider outage, and the failure taxonomy is
	// built by reading these strings.
	if runErr != nil {
		r.Reason = "run failed: " + runErr.Error()
		return r
	}

	if missing := missingCalls(t.MustCall, s); len(missing) > 0 {
		r.Reason = "never called: " + strings.Join(missing, ", ")
		return r
	}

	if t.Expect != "" && !matchExpect(answer, t.Expect) {
		r.Reason = fmt.Sprintf("answer does not contain %q", t.Expect)
		return r
	}

	r.Pass = true
	return r
}

// matchExpect checks whether expect is found in answer at word/token boundaries.
//
// Models format the same fact differently and may echo the prompt: a single-token
// expectation (e.g. "1") must match as an isolated token, not as an internal
// substring of another number (e.g. "10") or arithmetic fraction (e.g. "1/3").
func matchExpect(answer, expect string) bool {
	normAnswer := Normalise(answer)
	normExpect := Normalise(expect)
	if normExpect == "" {
		return true
	}

	start := 0
	for {
		idx := strings.Index(normAnswer[start:], normExpect)
		if idx < 0 {
			return false
		}
		pos := start + idx
		end := pos + len(normExpect)

		if isBounded(normAnswer, normExpect, pos, end) {
			return true
		}
		start = pos + 1
	}
}

func isBounded(s, expect string, pos, end int) bool {
	if pos > 0 {
		prev := s[pos-1]
		if isWordOrDigit(prev) || prev == '/' {
			return false
		}
		if prev == '.' && isDigit(expect[0]) {
			return false
		}
	}

	if end < len(s) {
		next := s[end]
		if isWordOrDigit(next) || next == '/' {
			return false
		}
		if next == '.' && end+1 < len(s) && isDigit(s[end+1]) {
			return false
		}
	}

	return true
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isWordOrDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// missingCalls reports which of want never appears as a tool_use block.
func missingCalls(want []string, s *loop.State) []string {
	if len(want) == 0 {
		return nil
	}
	called := map[string]bool{}
	if s != nil {
		for _, m := range s.Messages {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolUse {
					called[b.Name] = true
				}
			}
		}
	}

	var missing []string
	for _, name := range want {
		if !called[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// Normalise strips the differences that are formatting rather than meaning.
//
// Models disagree about presentation of the same fact: deepseek and ling answer
// "15% of 240 is **36**" where gemini answers "15% of 240 is 36." Exact matching
// would score that as wrong and send you looking for a bug in the agent.
//
// Known gap: thousands separators and trailing zeros are not handled, so "1,000"
// and "1000" still differ. Decide that when a task needs it rather than guessing
// at a rule now.
func Normalise(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("*", "", "_", "", "`", "", " ", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// NewScorecard aggregates results. Total cost comes from the runs themselves,
// so it is what the provider charged rather than a table's estimate.
func NewScorecard(model, commit string, results []Result) Scorecard {
	sc := Scorecard{
		Model:   model,
		Commit:  commit,
		When:    time.Now().UTC(),
		Results: results,
		Total:   len(results),
	}
	for _, r := range results {
		if r.Pass {
			sc.Passed++
		}
		sc.TotalCost += r.Cost
	}
	if sc.Total > 0 {
		sc.PassRate = float64(sc.Passed) / float64(sc.Total)
	}
	return sc
}

// LoadTasks reads a task set from disk.
func LoadTasks(path string) ([]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read tasks: %w", err)
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("eval: decode tasks %s: %w", path, err)
	}

	seen := map[string]bool{}
	for i, t := range tasks {
		if t.ID == "" {
			return nil, fmt.Errorf("eval: task %d has no id", i)
		}
		if seen[t.ID] {
			// Duplicate ids would silently overwrite each other in any
			// per-task comparison across scorecards.
			return nil, fmt.Errorf("eval: duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return tasks, nil
}

// WriteTable prints a scorecard as a fixed-width table.
func (sc Scorecard) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "%-38s %6s %9s %10s\n", "model", "pass", "cost", "$/task")
	perTask := 0.0
	if sc.Total > 0 {
		perTask = sc.TotalCost / float64(sc.Total)
	}
	fmt.Fprintf(w, "%-38s %2d/%-3d %9.5f %10.6f\n",
		trunc(sc.Model, 38), sc.Passed, sc.Total, sc.TotalCost, perTask)

	for _, r := range sc.Results {
		if !r.Pass {
			fmt.Fprintf(w, "  FAIL %-12s %s\n", r.TaskID, r.Reason)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
