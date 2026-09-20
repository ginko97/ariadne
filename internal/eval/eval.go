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
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

	// ExpectAll are facts that must all be in the answer, for work with no
	// single right wording: a summary of a book must name what its last
	// chapters are about, which only a model that read them can.
	ExpectAll []string `json:"expect_all,omitempty"`

	// MustNotCall names tools that must not even be requested — a document
	// that tells the model to write a file must not get a write_file call,
	// whether or not the call would have been approved.
	MustNotCall []string `json:"must_not_call,omitempty"`

	// Approve names the tools whose approval cards are answered yes. Every
	// other card is answered no: an eval approves nothing it was not told to,
	// and never reaches the web unless a task says so.
	Approve []string `json:"approve,omitempty"`

	// Files are fixtures written into the task's own folder, name to text.
	// FilesFrom copies a folder next to the task file instead, for binary
	// documents; inline Files are written after it. See files.go.
	Files     map[string]string `json:"files,omitempty"`
	FilesFrom string            `json:"files_from,omitempty"`

	// Checks on the folder after the run. ExpectFile is a file's whole
	// content, FileContains a part of it (both ignoring CRLF and trailing
	// newlines); Unchanged fixtures must be byte for byte what they were;
	// Absent files must not exist.
	ExpectFile   map[string]string `json:"expect_file,omitempty"`
	FileContains map[string]string `json:"file_contains,omitempty"`
	Unchanged    []string          `json:"unchanged,omitempty"`
	Absent       []string          `json:"absent,omitempty"`

	// dir is the folder of the task file, which FilesFrom is relative to.
	dir string
}

// usesFiles says whether t needs a folder of its own.
func (t Task) usesFiles() bool {
	return len(t.Files) > 0 || t.FilesFrom != "" || len(t.ExpectFile) > 0 ||
		len(t.FileContains) > 0 || len(t.Unchanged) > 0 || len(t.Absent) > 0
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
	// Provider is the backend that served this task, where the gateway reports
	// one. Two sweeps of one model id may not have run on the same thing —
	// backends differ in quantisation, context handling and latency — and this
	// project has already recorded pass-rate variance it could not attribute
	// and latency variance it called provider-side without being able to see
	// the provider. Recorded so the question can at least be asked.
	Provider string `json:"provider,omitempty"`
	// Attempts and Passes are set when a task was run more than once: it
	// passes only if every attempt did.
	Attempts int `json:"attempts,omitempty"`
	Passes   int `json:"passes,omitempty"`
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

// Score judges one finished run. It never *judges* on the model or the provider
// — only on what the run actually did — but it records which backend served it,
// because a comparison that cannot name what answered is comparing labels.
func Score(t Task, s *loop.State, answer string, runErr error) Result {
	r := Result{TaskID: t.ID, Answer: answer}
	if s != nil {
		r.Steps, r.Cost, r.RunID, r.Provider = s.Steps, s.Cost, s.RunID, s.Provider
	}

	// First, because the request itself is the failure: a forbidden call
	// made before the run went wrong for some other reason still happened.
	if forbidden := calledOf(t.MustNotCall, s); len(forbidden) > 0 {
		r.Reason = "called what it must not: " + strings.Join(forbidden, ", ")
		return r
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
	for _, e := range t.ExpectAll {
		if !matchExpect(answer, e) {
			r.Reason = fmt.Sprintf("answer does not contain %q", e)
			return r
		}
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
		prev, _ := utf8.DecodeLastRuneInString(s[:pos])
		if isWordRune(prev) || prev == '/' || prev == '-' {
			return false
		}
		if prev == '.' && len(expect) > 0 {
			first, _ := utf8.DecodeRuneInString(expect)
			if unicode.IsDigit(first) {
				return false
			}
		}
	}

	if end < len(s) {
		next, size := utf8.DecodeRuneInString(s[end:])
		if isWordRune(next) || next == '/' || next == '-' {
			return false
		}
		if next == '.' && end+size < len(s) {
			afterDot, _ := utf8.DecodeRuneInString(s[end+size:])
			if unicode.IsDigit(afterDot) {
				return false
			}
		}
	}

	return true
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// calledOf reports which of names were requested.
func calledOf(names []string, s *loop.State) []string {
	if len(names) == 0 {
		return nil
	}
	called := calledTools(s)
	var out []string
	for _, n := range names {
		if called[n] {
			out = append(out, n)
		}
	}
	return out
}

// missingCalls reports which of want was never requested, either as a tool_use
// block still in the conversation or as a call compaction recorded in the digest.
func missingCalls(want []string, s *loop.State) []string {
	if len(want) == 0 {
		return nil
	}
	called := calledTools(s)
	var missing []string
	for _, name := range want {
		if !called[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// calledTools is every tool requested in the run.
func calledTools(s *loop.State) map[string]bool {
	called := map[string]bool{}
	if s != nil {
		// Calls compaction dropped survive only as digest lines. Without them a
		// run that grew long enough to compact fails "never called" for a tool
		// it did call. The parse lives in loop, next to the writer.
		for _, name := range s.CompactedCalls() {
			called[name] = true
		}

		for _, m := range s.Messages {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolUse {
					called[b.Name] = true
				}
			}
		}
	}
	return called
}

// Normalise strips the differences that are formatting rather than meaning.
//
// Models disagree about presentation of the same fact. All three of these came
// from real runs marked as failures while being correct:
//
//	"15% of 240 is **36**"   markdown emphasis
//	"$1,157.625"             thousands separators
//	"$74.50"                 a trailing zero on a decimal
//
// The last two are why digits are handled and not just punctuation: 74.50 and
// 74.5 are the same number, and a matcher that disagrees sends you hunting for
// a bug in the agent that is not there.
func Normalise(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("*", "", "_", "", "`", "").Replace(s)
	s = stripDigitGroupSeparators(s)
	s = trimDecimalZeros(s)
	return strings.Join(strings.Fields(s), " ")
}

// stripDigitGroupSeparators removes a comma sitting between two digits, so
// "1,157.625" becomes "1157.625". A comma anywhere else is punctuation and
// stays — "for 1, 2 and 3" must not become "for 12 and 3".
func stripDigitGroupSeparators(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ',' && i > 0 && i+1 < len(s) && isDigit(s[i-1]) && isDigit(s[i+1]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// trimDecimalZeros rewrites "74.50" as "74.5" and "3.000" as "3", leaving
// anything that is not a decimal number alone.
//
// Known cost: "version 1.10" becomes "version 1.1". Version strings are not
// what this scorer compares, and the alternative is failing correct numeric
// answers, which is the worse of the two.
func trimDecimalZeros(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if !isDigit(s[i]) || (i > 0 && (isDigit(s[i-1]) || s[i-1] == '.')) {
			b.WriteByte(s[i])
			i++
			continue
		}

		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if j >= len(s) || s[j] != '.' {
			b.WriteString(s[i:j])
			i = j
			continue
		}

		k := j + 1
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j+1 { // "12." with no fraction is a sentence, not a decimal
			b.WriteString(s[i:j])
			i = j
			continue
		}

		frac := strings.TrimRight(s[j+1:k], "0")
		b.WriteString(s[i:j])
		if frac != "" {
			b.WriteByte('.')
			b.WriteString(frac)
		}
		i = k
	}
	return b.String()
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

	dir := filepath.Dir(path)
	seen := map[string]bool{}
	for i, t := range tasks {
		tasks[i].dir = dir
		if t.FilesFrom != "" {
			if err := safeRel(t.FilesFrom); err != nil {
				return nil, fmt.Errorf("eval: task %q: files_from: %w", t.ID, err)
			}
		}
		for name := range t.Files {
			if err := safeRel(name); err != nil {
				return nil, fmt.Errorf("eval: task %q: files: %w", t.ID, err)
			}
		}
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
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
