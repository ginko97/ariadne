package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTrace lays down one run's trace file, lines verbatim, so a test can
// include a deliberately broken one.
func writeTrace(t *testing.T, dir, runID string, lines ...string) {
	t.Helper()
	d := filepath.Join(dir, runID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(d, "trace.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	evStart  = `{"run_id":"r","seq":1,"kind":"run_start","text":"summarise the invoice"}`
	evReq    = `{"run_id":"r","seq":2,"kind":"request","model":"m"}`
	evResp   = `{"run_id":"r","seq":3,"kind":"response","stop":"tool_use","input_tokens":52,"output_tokens":18,"cost_usd":0.0001}`
	evCall   = `{"run_id":"r","seq":4,"kind":"tool_call","tool":"calc","args":{"expr":"2+2"}}`
	evResult = `{"run_id":"r","seq":5,"kind":"tool_result","tool":"calc","content":"4"}`
	evFetch  = `{"run_id":"r","seq":6,"kind":"tool_result","tool":"fetch","content":"api_token = sk-EXAMPLE"}`
	evDenied = `{"run_id":"r","seq":7,"kind":"tool_denied","tool":"write_file","content":"not permitted","is_error":true}`
	evRetry  = `{"run_id":"r","seq":8,"kind":"retry","content":"http 429","latency_ms":20000,"is_error":true}`
	evEndOK  = `{"run_id":"r","seq":9,"kind":"run_end","step":3,"cost_usd":0.0002}`
	evEndBad = `{"run_id":"r","seq":9,"kind":"run_end","step":1,"error":"loop: step limit exceeded"}`
)

func corpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Ids begin with a sortable timestamp; these are ordered oldest to newest.
	writeTrace(t, dir, "run_20260101T000000_aaa", evStart, evReq, evResp, evCall, evResult, evEndOK)
	writeTrace(t, dir, "run_20260102T000000_bbb", evStart, evFetch, evDenied, evEndBad)
	writeTrace(t, dir, "run_20260103T000000_ccc", evStart, evRetry, evEndOK)
	return dir
}

func TestSearchFiltersByKindAndTool(t *testing.T) {
	dir := corpus(t)

	got, err := Search(dir, Query{Kinds: []string{"tool_result"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kind filter returned %d, want 2", len(got))
	}

	got, err = Search(dir, Query{Kinds: []string{"tool_result"}, Tools: []string{"fetch"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Kinds and tools are AND across fields: a fetch result, not every result
	// and every fetch.
	if len(got) != 1 || got[0].Event.Tool != "fetch" {
		t.Errorf("kind+tool returned %+v", got)
	}
}

// The text query is the one that replaces reaching for grep, so it has to look
// in every field that can carry content rather than just one.
func TestSearchTextSpansContentFields(t *testing.T) {
	dir := corpus(t)

	for _, tc := range []struct{ q, where string }{
		{"sk-EXAMPLE", "content"},
		{"summarise the invoice", "text"},
		{"step limit", "error"},
		{"2+2", "args"},
	} {
		got, err := Search(dir, Query{Text: tc.q}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("%q not found; the %s field is not searched", tc.q, tc.where)
		}
	}

	// Case-insensitive, or half the searches a person types find nothing.
	got, _ := Search(dir, Query{Text: "SUMMARISE THE INVOICE"}, 0)
	if len(got) != 3 {
		t.Errorf("case-insensitive search returned %d, want 3", len(got))
	}
}

func TestSearchErrorsOnly(t *testing.T) {
	dir := corpus(t)

	got, err := Search(dir, Query{Errors: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The denial, the retry, and the run that ended badly.
	if len(got) != 3 {
		t.Fatalf("errors filter returned %d, want 3: %+v", len(got), got)
	}
	for _, m := range got {
		if !m.Event.IsError && m.Event.Error == "" {
			t.Errorf("non-error event matched: %+v", m.Event)
		}
	}
}

// Newest first, because the question is nearly always about what just
// happened, and a limit should cut the oldest rather than the most relevant.
func TestSearchIsNewestRunFirst(t *testing.T) {
	dir := corpus(t)

	got, err := Search(dir, Query{Kinds: []string{"run_start"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d runs", len(got))
	}
	if !strings.HasSuffix(got[0].RunID, "ccc") || !strings.HasSuffix(got[2].RunID, "aaa") {
		t.Errorf("order = %s, %s, %s", got[0].RunID, got[1].RunID, got[2].RunID)
	}

	// And a limit takes from the newest end.
	got, _ = Search(dir, Query{Kinds: []string{"run_start"}}, 1)
	if len(got) != 1 || !strings.HasSuffix(got[0].RunID, "ccc") {
		t.Errorf("limited search returned %+v", got)
	}
}

func TestSearchByPartialRunID(t *testing.T) {
	dir := corpus(t)

	got, err := Search(dir, Query{RunID: "bbb"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("run filter returned %d events, want 4", len(got))
	}
	for _, m := range got {
		if !strings.HasSuffix(m.RunID, "bbb") {
			t.Errorf("run filter leaked %s", m.RunID)
		}
	}
}

// A trace is append-only and a run can be killed mid-write, so the last line
// may be half a JSON object. Everything before it must still be readable —
// those are exactly the events worth having after a crash.
func TestSearchToleratesATruncatedTail(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "run_20260101T000000_aaa", evStart, evCall, `{"kind":"tool_res`)

	got, err := Search(dir, Query{}, 0)
	if err != nil {
		t.Fatalf("a truncated tail should not be an error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d events before the damage, want 2", len(got))
	}
}

// A corrupt line in the *middle* must not hide everything after it. An earlier
// version stopped at the first unparseable line, treating any damage as a tail,
// so one bad line silently truncated the whole file for the reader.
func TestSearchKeepsReadingPastACorruptLine(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "run_20260101T000000_aaa",
		evStart, `{ this is not json }`, evCall, evResult, evEndOK)

	got, err := Search(dir, Query{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: a corrupt line hid the rest", len(got))
	}

	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	// Counted rather than swallowed: analysis keeps working and the number says
	// the file is damaged.
	if st.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", st.Malformed)
	}
}

func TestSummariseAggregates(t *testing.T) {
	dir := corpus(t)

	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}

	if st.Runs != 3 {
		t.Errorf("Runs = %d, want 3", st.Runs)
	}
	if st.ByKind["tool_result"] != 2 || st.ByTool["calc"] != 2 {
		t.Errorf("ByKind=%v ByTool=%v", st.ByKind, st.ByTool)
	}
	if st.Denied != 1 {
		t.Errorf("Denied = %d, want 1", st.Denied)
	}
	if st.Retried != 1 || st.RetryMS != 20000 {
		t.Errorf("Retried=%d RetryMS=%d", st.Retried, st.RetryMS)
	}
	if len(st.Failed) != 1 || !strings.HasSuffix(st.Failed[0], "bbb") {
		t.Errorf("Failed = %v, want the run that ended with an error", st.Failed)
	}
	if st.Steps != 3+1+3 {
		t.Errorf("Steps = %d", st.Steps)
	}
}

// A SIGKILL leaves no run_end at all, so a killed run is invisible to Failed —
// it never got to say anything went wrong. For a runtime whose headline claim
// is surviving kill -9, that is the list most worth having.
func TestSummariseFindsRunsThatNeverEnded(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "run_20260101T000000_ok", evStart, evReq, evEndOK)
	writeTrace(t, dir, "run_20260102T000000_killed", evStart, evReq) // no run_end
	// A resumed run appends to the same file, so several starts and ends are
	// normal and must not be mistaken for damage.
	writeTrace(t, dir, "run_20260103T000000_resumed", evStart, evEndOK, evStart, evEndOK)

	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Incomplete) != 1 || !strings.HasSuffix(st.Incomplete[0], "killed") {
		t.Errorf("Incomplete = %v, want only the killed run", st.Incomplete)
	}
	if len(st.Failed) != 0 {
		t.Errorf("Failed = %v; a killed run did not fail, it stopped", st.Failed)
	}
}

// A directory holding a checkpoint but no trace is not an error.
func TestSearchSkipsRunsWithoutATrace(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "run_20260101T000000_aaa", evStart)
	if err := os.MkdirAll(filepath.Join(dir, "run_20260102T000000_bbb"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Search(dir, Query{}, 0)
	if err != nil {
		t.Fatalf("a run directory with no trace should be skipped, not fail: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d events", len(got))
	}
}

func TestSearchMissingDirIsAnError(t *testing.T) {
	if _, err := Search(filepath.Join(t.TempDir(), "nope"), Query{}, 0); err == nil {
		t.Error("a missing runs directory should be reported, not silently empty")
	}
}

// Filtering by an event kind (such as run_start) or tool must not falsely
// mark completed runs as incomplete just because run_end was filtered out.
func TestSummariseFilteredQueryDoesNotFalselyMarkIncomplete(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "run_20260101T000000_ok", evStart, evReq, evEndOK)

	st, err := Summarise(dir, Query{Kinds: []string{"run_start"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Incomplete) != 0 {
		t.Errorf("Incomplete = %v, want 0 (completed run was falsely marked incomplete)", st.Incomplete)
	}
	if st.Steps != 3 {
		t.Errorf("Steps = %d, want 3", st.Steps)
	}
}

// A resumed run emits multiple run_end events (one per segment).
// Steps must be the run's cumulative steps, not the sum of each segment's steps.
func TestSummariseResumedRunDoesNotInflateSteps(t *testing.T) {
	dir := t.TempDir()
	evEndSeg1 := `{"run_id":"r","seq":3,"kind":"run_end","step":2,"error":"context canceled"}`
	evEndSeg2 := `{"run_id":"r","seq":6,"kind":"run_end","step":3}`
	writeTrace(t, dir, "run_20260101T000000_resumed", evStart, evEndSeg1, evStart, evEndSeg2)

	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Steps != 3 {
		t.Errorf("Steps = %d, want 3 (cumulative steps were double-counted across segments)", st.Steps)
	}
}

// Free-text query must match CallID, so searching for a specific call finds
// tool results and approvals.
func TestSearchMatchesCallID(t *testing.T) {
	dir := t.TempDir()
	evCallWithID := `{"run_id":"r","seq":1,"kind":"tool_call","call_id":"call_xyz123","tool":"calc","args":{}}`
	writeTrace(t, dir, "run_20260101T000000_aaa", evCallWithID)

	got, err := Search(dir, Query{Text: "call_xyz123"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1 for call_id search", len(got))
	}
}

// A resumed run appends segments to one trace, and the last segment is the
// run's result: an earlier failure that a later resume finished is not a
// failure, and a later failure is one whatever came before it. A run that
// started a segment and never ended it is incomplete, and only that.
func TestSummariseLastSegmentDecidesFailure(t *testing.T) {
	endOK := `{"run_id":"r","seq":3,"kind":"run_end","step":2}`
	endErr := `{"run_id":"r","seq":3,"kind":"run_end","step":2,"error":"context canceled"}`

	for _, c := range []struct {
		name                   string
		events                 []string
		wantFailed, wantIncomp bool
	}{
		{"failed then resumed cleanly", []string{evStart, endErr, evStart, endOK}, false, false},
		{"succeeded then failed on resume", []string{evStart, endOK, evStart, endErr}, true, false},
		{"failed then crashed mid-resume", []string{evStart, endErr, evStart}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTrace(t, dir, "run_20260101T000000_seg", c.events...)
			st, err := Summarise(dir, Query{})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(st.Failed) == 1; got != c.wantFailed {
				t.Errorf("Failed = %v, want failed=%v", st.Failed, c.wantFailed)
			}
			if got := len(st.Incomplete) == 1; got != c.wantIncomp {
				t.Errorf("Incomplete = %v, want incomplete=%v", st.Incomplete, c.wantIncomp)
			}
		})
	}
}

// One line too long to decode is one malformed event, not a failed search.
// A 66 MB tool result from before fetch had a cap made `ariadne traces` fail
// with "bufio.Scanner: token too long" for every run on the machine.
func TestSearchSkipsALineTooLongToRead(t *testing.T) {
	saved := maxLine
	maxLine = 200
	defer func() { maxLine = saved }()

	dir := t.TempDir()
	huge := `{"run_id":"r","seq":2,"kind":"tool_result","content":"` + strings.Repeat("x", 1000) + `"}`
	writeTrace(t, dir, "run_20260101T000000_long", evStart, huge, evEndOK)
	// And one whose oversized line is the last thing in the file.
	writeTrace(t, dir, "run_20260101T000001_tail", evStart, huge)

	got, err := Search(dir, Query{}, 0)
	if err != nil {
		t.Fatalf("an oversized line failed the whole search: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d events, want the 3 readable ones around the long lines", len(got))
	}
	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Malformed != 2 {
		t.Errorf("Malformed = %d, want each oversized line counted once", st.Malformed)
	}
}

// Responses whose cost nobody measured are counted, so the total can be shown
// as a lower bound instead of passing for what the sweep cost.
func TestSummariseCountsUnpricedResponses(t *testing.T) {
	dir := t.TempDir()
	unpriced := `{"run_id":"r","seq":3,"kind":"response","stop":"end_turn","input_tokens":52,"output_tokens":18,"cost_unknown":true}`
	writeTrace(t, dir, "run_20260101T000000_mixed", evStart, evReq, evResp, evReq, unpriced, evEndOK)

	st, err := Summarise(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Unpriced != 1 {
		t.Errorf("Unpriced = %d, want 1", st.Unpriced)
	}
	if st.Cost != 0.0001 {
		t.Errorf("Cost = %v, want the one measured 0.0001", st.Cost)
	}
}
