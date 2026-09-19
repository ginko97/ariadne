package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Searching traces, and why this is not a tool the agent can call.
//
// The curriculum this project follows suggests search_traces as something the
// agent itself may use. It is not, and the reason is in the postmortem: a trace
// holds every byte a run ever saw, including fetched documents and — proven, in
// testdata/traces/exfiltration-success.jsonl — credentials that leaked into an
// answer. A tool that searches every past run is a read channel from any run
// into any other, which is a fresh exfiltration surface and a much better one
// than the attack that already worked, because it needs no injection at all.
//
// So this is for the person doing error analysis. That is also where the value
// has actually been: every finding in docs/eval-findings.md and
// docs/injection-postmortem.md came from reading a trace by hand.

// Query selects events. A zero Query matches everything.
type Query struct {
	// RunID limits the search to one run. A substring is enough, so a partial
	// id off the end of a run line works without copying the whole thing.
	RunID string
	// Kinds and Tools are OR within a field and AND across them: kinds
	// {tool_call} with tools {calc,fetch} means "a call to either tool".
	Kinds []string
	Tools []string
	// Text is a case-insensitive substring, matched against every field that
	// can carry content — the answer, a tool result, arguments, an error.
	// Searching one field at a time is the thing grep already does well.
	Text string
	// Errors keeps only events that record something going wrong: a failed
	// tool, a denial, a refused approval, a run that ended badly.
	Errors bool
}

// Match is one event and the run it came from.
type Match struct {
	RunID string
	Event Event
}

func (q Query) matches(e Event) bool {
	if len(q.Kinds) > 0 && !containsFold(q.Kinds, e.Kind) {
		return false
	}
	if len(q.Tools) > 0 && !containsFold(q.Tools, e.Tool) {
		return false
	}
	if q.Errors && !e.IsError && e.Error == "" {
		return false
	}
	if q.Text != "" {
		hay := strings.ToLower(strings.Join([]string{
			e.Text, e.Content, e.Error, string(e.Args), e.Tool, e.Model, e.Stop, e.CallID,
		}, "\x00"))
		if !strings.Contains(hay, strings.ToLower(q.Text)) {
			return false
		}
	}
	return true
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// runDirs lists run directories under dir, newest first.
//
// Newest first because the question is almost always about what just happened,
// and a limit should cut off the oldest rather than the most relevant.
func runDirs(dir string, runID string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("trace: read %s: %w", dir, err)
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if runID != "" && !strings.Contains(e.Name(), runID) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "trace.jsonl")); err != nil {
			continue // a run directory with a checkpoint but no trace
		}
		out = append(out, e.Name())
	}
	// Run ids begin with a sortable UTC timestamp, so this is chronological
	// without stat-ing anything.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// scan reads one trace file, calling fn for every event that parses, and
// returns how many lines did not.
//
// A truncated final line is normal rather than exceptional: the trace is
// append-only and a run can be killed mid-write, which is a case this project
// creates on purpose. But an unparseable line is only *probably* the tail, and
// an earlier version of this stopped reading at the first one — so a single
// corrupt line in the middle would hide every event after it, and the caller
// would see a short result rather than an error. Skipping and counting is the
// honest version: analysis keeps working, and the count says the file is
// damaged.
func scan(path string, fn func(Event) bool) (bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("trace: open %s: %w", path, err)
	}
	defer f.Close()

	// A bufio.Reader rather than a Scanner, because a Scanner stops at the
	// first line longer than its buffer and reports ErrTooLong for the whole
	// file. One such line existed: a 66 MB tool result from before fetch had a
	// size cap, which made `ariadne traces` fail for every run on the machine.
	// Now an oversized line is skipped and counted as malformed, like any
	// other line that cannot be read, and the rest of the file is still read.
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, tooLong, err := readLine(r, maxLine)
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return bad, nil
			}
			return bad, err
		}
		if tooLong {
			bad++
			continue
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			bad++
			continue
		}
		if !fn(e) {
			return bad, nil
		}
	}
}

// maxLine is the longest trace line scan will decode. A variable so a test can
// exercise the limit without writing megabytes.
var maxLine = 8 << 20

// readLine returns the next line without its newline. A line longer than max
// is read to its end and discarded, with tooLong set, so the next call starts
// on the following line.
func readLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			// A non-nil line with tooLong set, so the caller counts it even
			// when the oversized line is the last one in the file.
			return []byte{0}, true, err
		}
		return []byte(strings.TrimRight(string(buf), "\r\n")), false, err
	}
}

// Search returns matching events across every run under dir, newest run first.
//
// limit caps the number of matches, not the number of files read; 0 means all.
func Search(dir string, q Query, limit int) ([]Match, error) {
	runs, err := runDirs(dir, q.RunID)
	if err != nil {
		return nil, err
	}

	var out []Match
	for _, r := range runs {
		if limit > 0 && len(out) >= limit {
			break
		}
		_, err := scan(filepath.Join(dir, r, "trace.jsonl"), func(e Event) bool {
			if q.matches(e) {
				out = append(out, Match{RunID: r, Event: e})
			}
			return limit <= 0 || len(out) < limit
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Stats is what a sweep of traces adds up to.
type Stats struct {
	Runs   int
	Events int
	Cost   float64
	Steps  int
	// Unpriced counts responses whose cost was not measured; while it is
	// above zero, Cost is a lower bound.
	Unpriced int

	ByKind map[string]int
	ByTool map[string]int

	// Failed names runs whose run_end carried an error. This is the list worth
	// reading first, and it is the one a summary line never shows.
	Failed []string
	// Incomplete names runs that started and never ended. A SIGKILL leaves no
	// run_end at all, so such a run is invisible to Failed — it never got to
	// say anything went wrong. This is the only place the difference shows, and
	// for a runtime whose headline claim is surviving kill -9 it is exactly the
	// list worth having.
	Incomplete []string
	// Denied and Retried are called out separately because they answer
	// different questions from "did it fail": what the agent tried to do and
	// was not allowed to, and how much of the wall time was not work.
	Denied  int
	Retried int
	// Malformed counts lines that did not parse, across every file read.
	Malformed int
	// RetryMS is time spent waiting on rate limits. The loop times the whole
	// provider call, so without subtracting this a throttled run is
	// indistinguishable from a slow model.
	RetryMS int64
}

// Summarise aggregates instead of listing. Same filter, different question.
func Summarise(dir string, q Query) (Stats, error) {
	runs, err := runDirs(dir, q.RunID)
	if err != nil {
		return Stats{}, err
	}

	st := Stats{ByKind: map[string]int{}, ByTool: map[string]int{}}
	for _, r := range runs {
		seen := false
		starts, ends := 0, 0
		runSteps := 0
		runFailed := false
		bad, err := scan(filepath.Join(dir, r, "trace.jsonl"), func(e Event) bool {
			// Run-level lifecycle invariants hold across the whole file, regardless
			// of what the event filter selects.
			switch e.Kind {
			case KindRunStart:
				starts++
			case KindRunEnd:
				ends++
				if e.Step > runSteps {
					runSteps = e.Step
				}
				runFailed = (e.Error != "")
			}

			if !q.matches(e) {
				return true
			}
			seen = true
			st.Events++
			st.ByKind[e.Kind]++
			if e.Tool != "" {
				st.ByTool[e.Tool]++
			}
			switch e.Kind {
			case KindToolDenied:
				st.Denied++
			case KindRetry:
				st.Retried++
				st.RetryMS += e.LatencyMS
			case KindResponse:
				st.Cost += e.Cost
				if e.CostUnknown {
					st.Unpriced++
				}
			}
			return true
		})
		if err != nil {
			return st, err
		}
		st.Malformed += bad
		if seen {
			st.Runs++
			st.Steps += runSteps
			// A resumed run appends to the same file, so several starts and ends
			// are normal. What is not is more starts than ends.
			if starts > ends {
				st.Incomplete = append(st.Incomplete, r)
			} else if runFailed {
				st.Failed = append(st.Failed, r)
			}
		}
	}
	return st, nil
}
