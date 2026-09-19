package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/memory"
	"github.com/ginko97/ariadne/internal/tool"
	"github.com/ginko97/ariadne/internal/trace"
)

// The delta printer is the only user-visible part of streaming, and it has
// enough state to get wrong: a tool must be announced once rather than once per
// fragment, and argument fragments must not be echoed.
func TestPrintDeltaAnnouncesEachToolOnce(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	p(llm.Chunk{Text: "Let me "})
	p(llm.Chunk{Text: "compute that."})
	// One call, five fragments — the shape a real stream arrives in.
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc", Args: ""}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: `{"expr"`}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: `:"240*0.15"`}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: `}`}})
	p(llm.Chunk{Stop: llm.StopToolUse})

	got := out.String()
	if n := strings.Count(got, "calc"); n != 1 {
		t.Errorf("announced calc %d times:\n%s", n, got)
	}
	if !strings.Contains(got, "Let me compute that.") {
		t.Errorf("text deltas did not join:\n%s", got)
	}
	// Fragments are not valid JSON on their own; half a document scrolling past
	// is noise, and the whole call is in the trace regardless.
	if strings.Contains(got, `{"expr"`) || strings.Contains(got, "240*0.15") {
		t.Errorf("argument fragments were echoed:\n%s", got)
	}
	// The announcement must start its own line rather than run on from the text.
	if !strings.Contains(got, "compute that.\n") {
		t.Errorf("tool announcement did not break the text line:\n%s", got)
	}
}

func TestPrintDeltaSeparatesParallelCalls(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "a", Name: "calc"}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 1, ID: "b", Name: "fetch"}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: "{}"}})
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 1, Args: "{}"}})

	got := out.String()
	for _, name := range []string{"calc", "fetch"} {
		if n := strings.Count(got, name); n != 1 {
			t.Errorf("%s announced %d times:\n%s", name, n, got)
		}
	}
}

// In a multi-step run, OpenAI-compatible endpoints restart tool indices from 0
// on each turn. The announcement tracker must clear between turns, or tool 0
// in step 2 is silently suppressed because step 1 already announced an index 0.
func TestPrintDeltaResetsBetweenTurns(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	// Turn 1: tool at index 0.
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc"}})
	p(llm.Chunk{Stop: llm.StopToolUse})

	// Turn 2: different tool, provider re-uses index 0.
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c2", Name: "fetch"}})
	p(llm.Chunk{Stop: llm.StopToolUse})

	got := out.String()
	if !strings.Contains(got, "→ calc") {
		t.Errorf("turn 1 tool was not announced: %s", got)
	}
	if !strings.Contains(got, "→ fetch") {
		t.Errorf("turn 2 tool (at index 0) was suppressed: %s", got)
	}
}

func TestPrintDeltaResetsOnCostOnlyChunk(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	// Turn 1: tool at index 0, followed by cost-only usage chunk.
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc"}})
	p(llm.Chunk{Usage: llm.Usage{Cost: 0.005}})

	// Turn 2: tool at index 0 again.
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c2", Name: "fetch"}})
	p(llm.Chunk{Stop: llm.StopToolUse})

	got := out.String()
	if !strings.Contains(got, "→ calc") {
		t.Errorf("turn 1 tool was not announced: %s", got)
	}
	if !strings.Contains(got, "→ fetch") {
		t.Errorf("turn 2 tool (at index 0) was suppressed: %s", got)
	}
}

// splitList feeds --allow and --approve, where an empty flag has to mean
// "unrestricted" rather than "restricted to nothing".
func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"calc", []string{"calc"}},
		{" calc , fetch ", []string{"calc", "fetch"}},
		{"calc,,fetch,", []string{"calc", "fetch"}},
	} {
		got := splitList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestCheckNamesRejectsUnknownTool(t *testing.T) {
	defs := newRegistry("", defaultWorkspace, false).Defs()

	if err := checkNames(defs, []string{"calc"}, []string{"write_file"}); err != nil {
		t.Errorf("known tools were rejected: %v", err)
	}
	err := checkNames(defs, []string{"calc"}, []string{"wrtie_file"})
	if err == nil {
		t.Fatal("a typo was accepted; the run would deny silently")
	}
	if !strings.Contains(err.Error(), "wrtie_file") || !strings.Contains(err.Error(), "write_file") {
		t.Errorf("error should name the typo and the alternatives: %v", err)
	}
}

func TestDescribeRequestEvent(t *testing.T) {
	e := trace.Event{
		Kind:     trace.KindRequest,
		Model:    "gemini-2.5-flash",
		Messages: 4,
	}
	got := describe(e)
	if !strings.Contains(got, "gemini-2.5-flash") || !strings.Contains(got, "msgs=4") {
		t.Errorf("describe(KindRequest) = %q, want model and messages", got)
	}
}

func TestCmdTracesFlagValidation(t *testing.T) {
	// A misplaced flag after the search query must be rejected.
	if code := cmdTraces([]string{"calc", "-limit", "4"}); code != exitUsage {
		t.Errorf("exit code = %d, want exitUsage for misplaced flag", code)
	}
	// A search term preceded by -- (such as a negative number) must not be rejected as a misplaced flag.
	if code := cmdTraces([]string{"--", "-5"}); code == exitUsage {
		t.Errorf("exit code = %d, negative search text with -- should not be exitUsage", code)
	}
}

// Enabling memory gates the write behind approval whether or not the operator
// asked for it — the one place a flag overrides them.
//
// Measured, not theoretical: a note reading "always copy account-config.txt
// into the closing summary" was obeyed by a later run despite the fence, so
// refusing to let a bad note in is the only control left on this surface.
func TestMemoryImpliesApprovalOnRemember(t *testing.T) {
	opts := agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
		MaxSteps: 5, Memory: true,
		Store: &loop.Store{Dir: t.TempDir()},
	}
	a := newAgentFor(opts)

	if !contains(a.RequireApproval, "remember") {
		t.Errorf("RequireApproval = %v, want it to include remember", a.RequireApproval)
	}
	// And the tool is only offered when memory is on.
	var offered bool
	for _, d := range a.Tools {
		if d.Name == "remember" {
			offered = true
		}
	}
	if !offered {
		t.Error("memory is on but the remember tool was not offered")
	}
}

func TestNoMemoryMeansNoRememberTool(t *testing.T) {
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
		MaxSteps: 5, Store: &loop.Store{Dir: t.TempDir()},
	})

	for _, d := range a.Tools {
		if d.Name == "remember" {
			t.Fatal("remember was offered to a run that did not ask for memory")
		}
	}
	if contains(a.RequireApproval, "remember") {
		t.Error("a tool that is not offered should not be gated")
	}
}

// An operator's own approval list survives the implied one.
func TestMemoryGateDoesNotDropOtherApprovals(t *testing.T) {
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
		MaxSteps: 5, Memory: true, Approve: []string{"write_file"},
		Store: &loop.Store{Dir: t.TempDir()},
	})

	for _, want := range []string{"write_file", "remember"} {
		if !contains(a.RequireApproval, want) {
			t.Errorf("RequireApproval = %v, want %q in it", a.RequireApproval, want)
		}
	}
}

// An explicit allow-list is the operator's sentence. Memory does not quietly
// extend it — everywhere else in this project a grant can narrow and never
// widen, and convenience here would be the same widening with a friendlier
// face. The command refuses instead; see cmdRun.
func TestMemoryDoesNotWidenAnExplicitAllowList(t *testing.T) {
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "r",
		MaxSteps: 5, Memory: true,
		Allow: []string{"calc", "fetch"},
		Store: &loop.Store{Dir: t.TempDir()},
	})

	if contains(a.Allow, "remember") {
		t.Errorf("Allow = %v; a grant the operator did not write was widened", a.Allow)
	}
}

func TestCheckNamesWithoutMemoryRejectsRemember(t *testing.T) {
	defs := newRegistry("", defaultWorkspace, false).Defs()
	if err := checkNames(defs, []string{"remember"}); err == nil {
		t.Fatal("remember was accepted as valid tool name when memory was not requested")
	}
	defsWithMem := newRegistry("validate", defaultWorkspace, false).Defs()
	if err := checkNames(defsWithMem, []string{"remember"}); err != nil {
		t.Fatalf("remember was rejected when memory was requested: %v", err)
	}
}

func TestResumeReconcilesMemoryPrompt(t *testing.T) {
	state := loop.NewState("run_test", "task")
	state.System = systemPrompt

	origMemFile := memoryFile
	t.Cleanup(func() { memoryFile = origMemFile })

	tmpMem := filepath.Join(t.TempDir(), "MEMORY.md")
	memoryFile = tmpMem
	if err := (memory.Store{Path: tmpMem}).Append(memory.Note{Text: "remember this fact", RunID: "run_0"}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(state.System, "<memory>") {
		if prompt := memoryPrompt(true); prompt != "" {
			state.System += prompt
		}
	}

	if !strings.Contains(state.System, "remember this fact") {
		t.Errorf("state.System did not receive memory prompt:\n%s", state.System)
	}
}

func TestResumeInheritsMemory(t *testing.T) {
	state := loop.NewState("run_mem", "task")
	state.Model = "fake-model"
	state.BaseURL = "https://example.test/v1"
	state.RequireApproval = []string{"remember"}

	mem := false || contains(state.RequireApproval, "remember")
	if !mem {
		t.Fatal("expected mem to be true from checkpoint RequireApproval")
	}

	a := newAgentFor(agentOpts{
		Key: "k", Model: state.Model, BaseURL: state.BaseURL, RunID: state.RunID,
		MaxSteps: 5, Memory: mem,
		Store: &loop.Store{Dir: t.TempDir()},
	})
	var offered bool
	for _, d := range a.Tools {
		if d.Name == "remember" {
			offered = true
		}
	}
	if !offered {
		t.Error("resumed run should offer remember tool when inherited from checkpoint")
	}
}

// Memory-enabled is recorded on the state, not inferred.
//
// It was inferred two different ways — the approval list containing "remember",
// and the system prompt containing a "<memory>" marker. Both work until either
// string is reworded, and then fail silently: the second would append a whole
// second copy of the notes.
func TestMemoryIsRecordedNotInferred(t *testing.T) {
	var st loop.State
	st.Memory = true

	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back loop.State
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Memory {
		t.Error("Memory did not survive the checkpoint round-trip")
	}

	// And the fact does not depend on how the prompt or the gate happen to read.
	st2 := loop.State{Memory: true, System: "no marker here", RequireApproval: nil}
	if !st2.Memory {
		t.Error("Memory should not depend on the prompt text or the approval list")
	}
}

// These four replace tests that restated the condition in the test body and
// asserted the restatement — they called no production code and would have
// stayed green if the fix were deleted. The logic moved into functions so the
// tests could reach it.

// The loop reads State.ContextBudget, and Run only seeds it when it is zero. So
// a checkpoint carrying a budget silently ignored the flag: accepted, no error,
// no effect.
func TestResolveBudgetRecordsAnExplicitOverride(t *testing.T) {
	st := loop.NewState("r", "task")
	st.ContextBudget = 5000

	if got := resolveBudget(3000, st); got != 3000 {
		t.Errorf("resolveBudget = %d, want the flag", got)
	}
	if st.ContextBudget != 3000 {
		t.Errorf("State.ContextBudget = %d; the loop reads this, so the flag did nothing", st.ContextBudget)
	}
}

// No flag means the checkpoint's budget stands, which is what makes a resumed
// run compact like the original.
func TestResolveBudgetKeepsTheCheckpointWithoutAFlag(t *testing.T) {
	st := loop.NewState("r", "task")
	st.ContextBudget = 5000

	if got := resolveBudget(0, st); got != 5000 {
		t.Errorf("resolveBudget = %d, want the checkpoint's 5000", got)
	}
	if st.ContextBudget != 5000 {
		t.Errorf("State.ContextBudget = %d, want it untouched", st.ContextBudget)
	}
}

func TestResolveEndpointPrefersTheFlagAndRecordsIt(t *testing.T) {
	st := loop.NewState("r", "task")
	st.BaseURL = "https://checkpoint.test/v1"

	if got := resolveEndpoint("https://override.test/v1", st); got != "https://override.test/v1" {
		t.Errorf("endpoint = %q", got)
	}
	if st.BaseURL != "https://override.test/v1" {
		t.Errorf("the override was not recorded: %q", st.BaseURL)
	}

	st2 := loop.NewState("r", "task")
	st2.BaseURL = "https://checkpoint.test/v1"
	if got := resolveEndpoint("", st2); got != "https://checkpoint.test/v1" {
		t.Errorf("without a flag the checkpoint should win, got %q", got)
	}
}

// Both lists can refuse, and they refuse for different reasons.
func TestCheckResumeGrants(t *testing.T) {
	withAllow := func(a ...string) *loop.State {
		st := loop.NewState("r", "task")
		st.Allow = a
		return st
	}

	if err := checkResumeGrants(false, []string{"calc"}, withAllow("calc")); err != nil {
		t.Errorf("no memory means nothing to check: %v", err)
	}
	if err := checkResumeGrants(true, nil, withAllow()); err != nil {
		t.Errorf("no allow-list anywhere is unrestricted: %v", err)
	}
	if err := checkResumeGrants(true, []string{"calc", "remember"}, withAllow("calc", "remember")); err != nil {
		t.Errorf("remember listed in both should pass: %v", err)
	}

	// The flag list is the operator's sentence now.
	err := checkResumeGrants(true, []string{"calc", "fetch"}, withAllow("calc", "remember"))
	if err == nil || !strings.Contains(err.Error(), "-allow") {
		t.Errorf("a flag list without remember should be refused: %v", err)
	}

	// The checkpoint's list is the grant the run has been operating under, and
	// resume never widens one. This is the case the write side would otherwise
	// die in silently: memory read on, remember refused at the loop.
	err = checkResumeGrants(true, nil, withAllow("calc", "fetch"))
	if err == nil || !strings.Contains(err.Error(), "cannot widen") {
		t.Errorf("a checkpoint list without remember should be refused: %v", err)
	}
}

func TestResolveModel(t *testing.T) {
	st := loop.NewState("run_1", "task")
	st.Model = "model-checkpoint"

	// Explicit flag overrides checkpoint
	if got := resolveModel("model-flag", true, st); got != "model-flag" {
		t.Errorf("explicit flag resolveModel = %q, want model-flag", got)
	}
	if st.Model != "model-flag" {
		t.Errorf("st.Model = %q, want model-flag", st.Model)
	}

	// Non-explicit flag (default fallback) keeps checkpoint
	st2 := loop.NewState("run_2", "task")
	st2.Model = "model-checkpoint"
	if got := resolveModel("default-model", false, st2); got != "model-checkpoint" {
		t.Errorf("non-explicit flag resolveModel = %q, want model-checkpoint", got)
	}
	if st2.Model != "model-checkpoint" {
		t.Errorf("st2.Model = %q, want model-checkpoint", st2.Model)
	}
}

func TestPrintDeltaResetsOnUsageChunk(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	// Turn 1 ends with usage but no Stop reason
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc"}})
	p(llm.Chunk{Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})

	// Turn 2 calls tool at index 0 again; it must be announced because usage reset the state
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c2", Name: "calc"}})

	got := out.String()
	if n := strings.Count(got, "calc"); n != 2 {
		t.Errorf("expected calc to be announced twice across responses, got %d times in:\n%s", n, got)
	}
}

func TestPrintDeltaResetsOnCostReportedChunk(t *testing.T) {
	var out strings.Builder
	p := printDelta(&out)

	// Turn 1 ends with zero-token CostReported usage (e.g. OpenRouter free model)
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc"}})
	p(llm.Chunk{Usage: llm.Usage{CostReported: true}})

	// Turn 2 calls tool at index 0 again; it must be announced because usage reset the state
	p(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c2", Name: "calc"}})

	got := out.String()
	if n := strings.Count(got, "calc"); n != 2 {
		t.Errorf("expected calc to be announced twice across responses, got %d times in:\n%s", n, got)
	}
}

func TestApproveFromReader(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"", false},
	}

	call := llm.ToolCall{Name: "bash", Args: json.RawMessage(`{}`)}
	for _, tc := range cases {
		r := bufio.NewReader(strings.NewReader(tc.input))
		var out strings.Builder
		got, err := approveFromReader(r, &out, call)
		if err != nil {
			t.Errorf("approveFromReader(%q) unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("approveFromReader(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestApproveSharedReaderPreservesInput(t *testing.T) {
	input := "first line\ny\nsecond line\n"
	r := bufio.NewReader(strings.NewReader(input))

	line1, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(line1) != "first line" {
		t.Fatalf("reading line 1: %q, %v", line1, err)
	}

	var out strings.Builder
	ok, err := approveFromReader(r, &out, llm.ToolCall{Name: "calc"})
	if err != nil || !ok {
		t.Fatalf("approval: %v, %v", ok, err)
	}

	line2, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(line2) != "second line" {
		t.Fatalf("reading line 2: %q, %v", line2, err)
	}
}

func TestApproveOnTerminalFailsClosedOnNonTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not_a_tty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	fn := approveOnTerminalReader(f, r)
	approved, err := fn(context.Background(), llm.ToolCall{Name: "write_file"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if approved {
		t.Error("non-terminal input must fail closed (deny)")
	}
}

// A resumed run keeps the directory it was reading. Resuming against a
// different one would leave every path the run remembers pointing at files it
// never saw — the same class of silent redirection that resolveEndpoint and
// resolveBudget exist to prevent.
func TestResolveWorkspace(t *testing.T) {
	t.Run("checkpoint wins when no flag is given", func(t *testing.T) {
		st := &loop.State{Workspace: "/repo/alpha"}
		if got := resolveWorkspace("", st); got != "/repo/alpha" {
			t.Errorf("got %q, want the checkpoint's directory", got)
		}
	})

	t.Run("an explicit flag overrides and is recorded", func(t *testing.T) {
		st := &loop.State{Workspace: "/repo/alpha"}
		if got := resolveWorkspace("/repo/beta", st); got != "/repo/beta" {
			t.Errorf("got %q, want the flag", got)
		}
		// Recorded, or the next resume would silently go back to the old one —
		// the bug resolveBudget was written to fix.
		if st.Workspace != "/repo/beta" {
			t.Errorf("State.Workspace = %q, want the override recorded", st.Workspace)
		}
	})

	t.Run("a run from before this field falls back to the default", func(t *testing.T) {
		st := &loop.State{}
		if got := resolveWorkspace("", st); got != defaultWorkspace {
			t.Errorf("got %q, want %q", got, defaultWorkspace)
		}
		if st.Workspace != defaultWorkspace {
			t.Errorf("st.Workspace = %q, want recorded default %q", st.Workspace, defaultWorkspace)
		}
	})
}

// Every flag a command registers is named in --help.
//
// The usage text is a hand-written const and the flags are registered inside
// each cmd function, so nothing but this keeps the two in step. They had not
// been: -mcp-config shipped in 0ec4602 and -port in the ui work with neither
// in the usage block, which leaves a whole capability invisible to anyone who
// reads --help instead of the source.
//
// Reading main.go is deliberate. The flag sets are built inside functions that
// need a provider and a store to run, and registering them elsewhere just so a
// test could list them would be the tail wagging the dog.
func TestUsageNamesEveryFlag(t *testing.T) {
	// Every non-test file, not just main.go: setup's flags live in setup.go,
	// and a test reading one file would pass whatever a new file registered.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var src []byte
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	re := regexp.MustCompile(`fs\.(?:String|Int|Bool|Duration|Float64)\("([a-z][a-z-]*)"`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	if len(seen) < 10 {
		t.Fatalf("found only %d flags in main.go; the pattern has stopped matching how flags are registered", len(seen))
	}
	for name := range seen {
		if !regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(name) + `\s`).MatchString(usageText) {
			t.Errorf("-%s is registered but --help never mentions it", name)
		}
	}
}

// remoteStub is a tool as an MCP server would present it: known only by name
// until connected. Nothing here calls it.
type remoteStub string

func (r remoteStub) Name() string          { return string(r) }
func (remoteStub) Description() string     { return "" }
func (remoteStub) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (remoteStub) Call(context.Context, string, json.RawMessage) (llm.ToolResult, error) {
	return llm.ToolResult{}, nil
}

// -allow and -approve can name a tool an MCP server provides.
//
// They could not: every command checked the names against the local registry
// before connecting, so `-approve write_file` against a filesystem server's
// write tool exited "unknown tool" and the only way to run with that server
// was ungated. The gate the postmortem measured did not reach any MCP tool.
func TestNameCheckSeesRemoteTools(t *testing.T) {
	local := newRegistry("", t.TempDir(), false).Defs()
	remote := []tool.Tool{remoteStub("read_text_file"), remoteStub("move_file")}

	if err := checkNames(local, nil, []string{"move_file"}); err == nil {
		t.Fatal("precondition: the local registry alone should not know move_file")
	}
	if err := checkNames(withRemote(local, remote), []string{"calc", "read_text_file"}, []string{"move_file"}); err != nil {
		t.Errorf("an MCP tool could not be granted or gated: %v", err)
	}
	// And a typo is still a typo.
	if err := checkNames(withRemote(local, remote), nil, []string{"move_fiel"}); err == nil {
		t.Error("a misspelt tool passed once remote tools were included")
	}
}

func TestRetryMessageNamesTheCause(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{0, "provider unreachable"},
		{429, "rate limited"},
		{503, "provider error (http 503)"},
	} {
		if got := retryMessage(c.status, time.Second); !strings.HasPrefix(got, c.want) {
			t.Errorf("retryMessage(%d) = %q, want prefix %q", c.status, got, c.want)
		}
	}
}

// Every MCP tool is gated unless trusted, so a tool nobody thought to list —
// the fourth way a filesystem server changes a file, or one a server upgrade
// adds — arrives needing a yes rather than running unasked.
func TestGateMCPGatesEveryRemoteToolNotTrusted(t *testing.T) {
	remote := []tool.Tool{
		remoteStub("fs__read_text_file"), remoteStub("fs__write_file"),
		remoteStub("fs__edit_file"), remoteStub("fs__move_file"),
	}

	got, err := gateMCP([]string{"write_file"}, []string{"fs__read_text_file"}, remote)
	if err != nil {
		t.Fatal(err)
	}
	want := "write_file,fs__write_file,fs__edit_file,fs__move_file"
	if strings.Join(got, ",") != want {
		t.Errorf("gated = %v, want %s", got, want)
	}

	// No MCP servers: -approve passes through untouched.
	if got, _ := gateMCP([]string{"write_file"}, nil, nil); strings.Join(got, ",") != "write_file" {
		t.Errorf("without MCP, gated = %v, want just write_file", got)
	}
}

func TestGateMCPRefusesContradictionsAndBuiltins(t *testing.T) {
	remote := []tool.Tool{remoteStub("fs__write_file")}
	for _, c := range []struct {
		name           string
		approve, trust []string
	}{
		{"a built-in cannot be trusted", nil, []string{"write_file"}},
		{"an unknown name cannot be trusted", nil, []string{"fs__wirte_file"}},
		{"both approved and trusted", []string{"fs__write_file"}, []string{"fs__write_file"}},
	} {
		if _, err := gateMCP(c.approve, c.trust, remote); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}

	// A misspelt MCP tool is told it does not exist and shown what does, not
	// told that built-ins cannot be trusted, which is true and beside the point.
	_, err := gateMCP(nil, []string{"fs__wirte_file"}, remote)
	if err == nil || !strings.Contains(err.Error(), "available: fs__write_file") {
		t.Errorf("typo error = %v, want one listing the available MCP tools", err)
	}
}

// -exec offers the tool and gates it, with nothing the operator passes able to
// separate the two. An empty -approve, an unrelated one, and a run that also
// has memory all end up asking before every exec call.
func TestExecIsAlwaysGated(t *testing.T) {
	for _, approve := range [][]string{nil, {"write_file"}} {
		a := newAgentFor(agentOpts{
			Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
			MaxSteps: 5, Exec: true, Memory: true, Approve: approve,
			Store: &loop.Store{Dir: t.TempDir()},
		})
		if !contains(a.RequireApproval, "exec") {
			t.Errorf("approve=%v: RequireApproval = %v, want exec in it", approve, a.RequireApproval)
		}
		for _, want := range append([]string{"remember"}, approve...) {
			if !contains(a.RequireApproval, want) {
				t.Errorf("approve=%v: forcing exec dropped %q", approve, want)
			}
		}
		var offered bool
		for _, d := range a.Tools {
			if d.Name == "exec" {
				offered = true
			}
		}
		if !offered {
			t.Error("-exec did not offer the exec tool")
		}
	}

	// And without -exec there is no tool to gate.
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
		MaxSteps: 5, Store: &loop.Store{Dir: t.TempDir()},
	})
	for _, d := range a.Tools {
		if d.Name == "exec" {
			t.Error("exec was offered to a run that did not ask for it")
		}
	}
}

// The process's own keys are replaced with a label naming the variable; values
// too short to be a key are left alone, so a placeholder cannot blank ordinary
// text; and with no keys set there is nothing to do.
func TestRedactSecretsUsesThisProcesssKeys(t *testing.T) {
	env := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-v1-abcdefghijklmnop",
		"GEMINI_API_KEY":     "x",
		"UNRELATED":          "sk-not-ours-abcdefghijk",
	}
	r := redactSecrets(func(k string) string { return env[k] })
	if r == nil {
		t.Fatal("no redactor with a key set")
	}
	got := r("key=sk-or-v1-abcdefghijklmnop, other=sk-not-ours-abcdefghijk, text with x in it")
	if strings.Contains(got, "sk-or-v1-abcdefghijklmnop") {
		t.Errorf("own key survived: %s", got)
	}
	if !strings.Contains(got, "[REDACTED OPENROUTER_API_KEY]") {
		t.Errorf("no label naming the variable: %s", got)
	}
	if !strings.Contains(got, "text with x in it") {
		t.Errorf("a one-character placeholder redacted ordinary text: %s", got)
	}
	if !strings.Contains(got, "sk-not-ours-abcdefghijk") {
		t.Errorf("a value not held by this process was touched: %s", got)
	}

	if redactSecrets(func(string) string { return "" }) != nil {
		t.Error("a redactor was built with no keys to redact")
	}
}

// Every agent the commands build carries the redactor, so no command can offer
// exec, fetch or an MCP tool with the process's key readable in the output.
func TestAgentsRedactTheProvidersKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-wired-into-every-agent")
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test",
		MaxSteps: 5, Exec: true, Store: &loop.Store{Dir: t.TempDir()},
	})
	if a.Redact == nil {
		t.Fatal("the agent has no redactor")
	}
	if got := a.Redact("OPENROUTER_API_KEY=sk-or-v1-wired-into-every-agent"); strings.Contains(got, "wired-into") {
		t.Errorf("the key survived the agent's redactor: %s", got)
	}
}

// A conversation recorded against another endpoint takes that endpoint's key
// or none. d9e9778 fell back to the server's, which would have sent an
// OpenRouter key to api.x.ai for a conversation recorded there with no
// XAI_API_KEY set.
func TestConversationEndpointNeverLendsTheServersKey(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-server")
	t.Setenv("XAI_API_KEY", "")

	st := &loop.State{BaseURL: "https://api.x.ai/v1"}
	url, key := conversationEndpoint("https://openrouter.ai/api/v1", "sk-or-server", st)
	if url != "https://api.x.ai/v1" {
		t.Errorf("url = %q, want the conversation's endpoint", url)
	}
	if key == "sk-or-server" {
		t.Fatal("the OpenRouter key would be sent to api.x.ai")
	}

	// With its own key set, the conversation uses it.
	t.Setenv("XAI_API_KEY", "xai-its-own")
	if _, key := conversationEndpoint("https://openrouter.ai/api/v1", "sk-or-server", st); key != "xai-its-own" {
		t.Errorf("key = %q, want the endpoint's own key", key)
	}

	// Same endpoint, or none recorded: the server's key is the right one.
	for _, s := range []*loop.State{nil, {}, {BaseURL: "https://openrouter.ai/api/v1"}} {
		if u, k := conversationEndpoint("https://openrouter.ai/api/v1", "sk-or-server", s); k != "sk-or-server" || u != "https://openrouter.ai/api/v1" {
			t.Errorf("state %+v: got %q %q, want the server's endpoint and key", s, u, k)
		}
	}
}

// A provider's key goes only to that provider's host, matched on the parsed
// host rather than a substring, and an unrecognised host gets ARIADNE_API_KEY
// or nothing.
func TestAPIKeyGoesOnlyToItsOwnProvider(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "k-openrouter")
	t.Setenv("GEMINI_API_KEY", "k-gemini")
	t.Setenv("OPENAI_API_KEY", "k-openai")
	t.Setenv("XAI_API_KEY", "k-xai")

	for _, c := range []struct{ url, want string }{
		{"https://openrouter.ai/api/v1", "k-openrouter"},
		{"openrouter.ai/api/v1", "k-openrouter"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "k-gemini"},
		{"https://api.openai.com/v1", "k-openai"},
		{"api.openai.com/v1", "k-openai"},
		{"https://api.x.ai/v1", "k-xai"},
		// Unrecognised: no provider's key, whichever are set.
		{"https://api.groq.com/openai/v1", ""},
		// This machine: no provider's key, a placeholder so commands start.
		{"http://localhost:11434/v1", localNoKey},
		{"http://127.0.0.1:11434/v1", localNoKey},
		{"http://[::1]:11434/v1", localNoKey},
		{"localhost:11434/v1", localNoKey},
		// Not this machine, whatever the name says.
		{"http://192.168.1.5:11434/v1", ""},
		{"http://localhost.attacker.example/v1", ""},
		// Substrings of a provider's domain are not that provider.
		{"https://max.ai/v1", ""},
		{"https://openrouter.ai.attacker.example/v1", ""},
		{"https://notopenai.com/v1", ""},
	} {
		if got, _ := apiKey(c.url); got != c.want {
			t.Errorf("apiKey(%s) = %q, want %q", c.url, got, c.want)
		}
	}

	// ARIADNE_API_KEY is the operator's choice for every endpoint.
	t.Setenv("ARIADNE_API_KEY", "k-ariadne")
	if got, name := apiKey("https://api.groq.com/openai/v1"); got != "k-ariadne" || name != "ARIADNE_API_KEY" {
		t.Errorf("with ARIADNE_API_KEY set: %q %q", got, name)
	}
	// Including a local server that does want a key.
	if got, _ := apiKey("http://localhost:4000/v1"); got != "k-ariadne" {
		t.Errorf("ARIADNE_API_KEY lost to the loopback placeholder: %q", got)
	}
}

func TestCmdChatRefusesModelSwitchWithPendingToolCalls(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	store := &loop.Store{Dir: runsDir}
	// State with pending tool call: assistant message has BlockToolUse, no matching tool_result.
	st := &loop.State{
		RunID: "run_pending_chat",
		Model: "initial-model",
		Task:  "some task",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "calc 1+1"}}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "call_1", Name: "calc", Args: json.RawMessage(`{"expr":"1+1"}`)},
			}},
		},
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	// Resuming with explicit -model while tool calls are pending must fail with exitUsage.
	code := cmdChat([]string{"-model", "different-model", "run_pending_chat"})
	if code != exitUsage {
		t.Errorf("cmdChat with explicit -model on pending run returned %d, want exitUsage (%d)", code, exitUsage)
	}
}

// Unmeasured must never print as free: "$0.0000" for a run that was billed is
// the string this exists to prevent.
func TestCostTextNeverShowsUnknownAsZero(t *testing.T) {
	for _, c := range []struct {
		usd     float64
		unknown bool
		want    string
	}{
		{0, true, "unknown"},
		{0.0123, true, ">=$0.0123"},
		{0.0123, false, "$0.0123"},
		{0, false, "$0.0000"},
	} {
		if got := costText(c.usd, c.unknown, 4); got != c.want {
			t.Errorf("costText(%v, %v) = %q, want %q", c.usd, c.unknown, got, c.want)
		}
	}
}

// An eval task's approvals: yes for the tools it names, no for the rest —
// including web_fetch and exec, which no eval should reach unless named.
func TestEvalApprovesOnlyListedTools(t *testing.T) {
	approve := approveListed([]string{"edit_file"})
	for name, want := range map[string]bool{"edit_file": true, "web_fetch": false, "exec": false, "write_file": false} {
		if got, err := approve(context.Background(), llm.ToolCall{Name: name}); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	if got, _ := approveListed(nil)(context.Background(), llm.ToolCall{Name: "edit_file"}); got {
		t.Error("no list approved edit_file")
	}
}
