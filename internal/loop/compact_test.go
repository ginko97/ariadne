package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// assertWellFormed is the invariant compaction exists to not break.
//
// Every tool_use must have a result and every result must have a call. Either
// half alone is a provider 400 — the failure mode of a naive sliding window
// here is not a worse answer, it is a run that stops working, and it shows up
// only against a real endpoint.
func assertWellFormed(t *testing.T, msgs []llm.Message) {
	t.Helper()

	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockToolUse:
				calls[b.ID] = true
			case llm.BlockToolResult:
				results[b.CallID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Errorf("tool_use %q has no result: the request is invalid", id)
		}
	}
	for id := range results {
		if !calls[id] {
			t.Errorf("tool_result %q has no call: the request is invalid", id)
		}
	}
}

// conversation builds n complete tool round trips after the task, each padded
// so the sizes are worth comparing.
func conversation(task string, n int) *State {
	s := NewState("run_test", task)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		s.Messages = append(s.Messages,
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: strings.Repeat("thinking ", 20)},
				{Type: llm.BlockToolUse, ID: id, Name: "fetch",
					Args: json.RawMessage(fmt.Sprintf(`{"path":"doc-%d.html"}`, i))},
			}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				{Type: llm.BlockToolResult, CallID: id,
					Content: strings.Repeat("x", 200) + fmt.Sprintf(" result-%d", i)},
			}},
		)
	}
	return s
}

func TestCompactNeverSplitsAToolPair(t *testing.T) {
	s := conversation("summarise the documents", 10)
	assertWellFormed(t, s.Messages)

	if n := compact(s, 100_000, 10_000); n == 0 {
		t.Fatal("nothing was dropped")
	}
	assertWellFormed(t, s.Messages)

	// And the survivors must still be in order, results after their call.
	for i, m := range s.Messages {
		if isToolResults(m) && i > 0 && !hasToolUse(s.Messages[i-1]) {
			t.Errorf("results at %d do not follow a call", i)
		}
	}
}

// An agent that forgets what it was asked has not been compacted.
func TestCompactKeepsTheTask(t *testing.T) {
	s := conversation("count the invoices in the workspace", 12)
	compact(s, 100_000, 5_000)

	if len(s.Messages) == 0 || s.Messages[0].Role != llm.RoleUser {
		t.Fatal("the task message is gone")
	}
	if got := s.Messages[0].Blocks[0].Text; got != "count the invoices in the workspace" {
		t.Errorf("block 0 of the task message is %q", got)
	}
	if s.Task != "count the invoices in the workspace" {
		t.Errorf("State.Task = %q", s.Task)
	}
}

// The digest is what stops compaction being amnesia: the model should still be
// able to see which tools it already ran and what came back.
func TestCompactLeavesADigestOfWhatWent(t *testing.T) {
	s := conversation("summarise", 10)
	dropped := compact(s, 100_000, 8_000)
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}

	if len(s.Messages[0].Blocks) != 2 {
		t.Fatalf("task message has %d blocks, want task + digest", len(s.Messages[0].Blocks))
	}
	digest := s.Messages[0].Blocks[1].Text
	if !strings.Contains(digest, "fetch(") {
		t.Errorf("digest does not name the dropped calls:\n%s", digest)
	}
	if !strings.Contains(digest, "result-0") {
		t.Errorf("digest does not carry what the calls returned:\n%s", digest)
	}
	if !strings.Contains(digest, fmt.Sprint(dropped)) {
		t.Errorf("digest does not say how much went:\n%s", digest)
	}
}

// A second compaction must not throw away what the first one recorded, and must
// not stack a second digest behind the first either.
func TestSecondCompactionAccumulatesOneDigest(t *testing.T) {
	s := conversation("summarise", 8)
	if compact(s, 100_000, 9_000) == 0 {
		t.Fatal("first compaction dropped nothing")
	}
	first := s.Messages[0].Blocks[1].Text

	// Grow it again, then compact again.
	s2 := conversation("summarise", 8)
	s.Messages = append(s.Messages, s2.Messages[1:]...)
	if compact(s, 100_000, 6_000) == 0 {
		t.Fatal("second compaction dropped nothing")
	}

	if len(s.Messages[0].Blocks) != 2 {
		t.Fatalf("task message has %d blocks; the digest stacked", len(s.Messages[0].Blocks))
	}
	digest := s.Messages[0].Blocks[1].Text
	if !strings.Contains(digest, fmt.Sprint(s.Dropped)) {
		t.Errorf("digest count is not cumulative (Dropped=%d):\n%s", s.Dropped, digest)
	}
	if strings.Count(digest, "earlier messages have been dropped") != 1 {
		t.Errorf("more than one digest header:\n%s", digest)
	}
	if len(digest) > maxDigestChars+64 {
		t.Errorf("digest is %d chars, past its cap", len(digest))
	}
	_ = first
}

// Under budget, nothing moves. A conversation that never approaches the window
// should never lose anything.
func TestCompactDoesNothingUnderBudget(t *testing.T) {
	s := conversation("short job", 3)
	before := len(s.Messages)

	if n := compact(s, 500, 10_000); n != 0 {
		t.Errorf("dropped %d messages while under budget", n)
	}
	if len(s.Messages) != before || len(s.Messages[0].Blocks) != 1 {
		t.Error("the conversation was modified under budget")
	}
	if n := compact(s, 500, 0); n != 0 {
		t.Errorf("dropped %d messages with compaction disabled", n)
	}
}

// Trimming to exactly the budget puts the run back over it on the very next
// step. The gap is what stops every step from being a compaction.
func TestCompactTrimsBelowTheBudgetNotOntoIt(t *testing.T) {
	s := conversation("summarise", 20)
	before := totalSize(s.Messages)

	compact(s, 40_000, 20_000) // twice over: roughly half must go
	after := totalSize(s.Messages)

	ratio := float64(after) / float64(before)
	if ratio > 0.5*compactTarget+0.15 {
		t.Errorf("kept %.0f%% of the conversation; expected roughly %.0f%%",
			ratio*100, 50*compactTarget)
	}
	if after == 0 {
		t.Error("everything went")
	}
}

// Even an impossible budget leaves the task and the last exchange. A
// conversation trimmed to nothing is not cheaper, it is a restart — the run
// would repeat work it has already paid for.
func TestCompactKeepsTheLastExchange(t *testing.T) {
	s := conversation("summarise", 6)
	compact(s, 1_000_000, 1)

	if len(s.Messages) < 3 {
		t.Fatalf("only %d messages left; task plus one exchange is the floor", len(s.Messages))
	}
	assertWellFormed(t, s.Messages)
}

// The loop compacts on the provider's number, not on a guess of its own.
func TestRunCompactsWhenTheProviderSaysThePromptIsTooBig(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		// A first turn that reports a prompt far past the budget.
		{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "still working"}},
			Stop:   llm.StopToolUse,
		},
		endResponse("done", 10, 10),
	}}
	// Give the first response a tool call so the conversation grows normally.
	fake.Responses[0].Blocks = append(fake.Responses[0].Blocks, llm.Block{
		Type: llm.BlockToolUse, ID: "c1", Name: "fetch", Args: json.RawMessage(`{}`),
	})
	fake.Responses[0].Usage = llm.Usage{InputTokens: 80_000, OutputTokens: 10}

	var events []trace.Event
	a := &Agent{
		Provider:      fake,
		Model:         "test",
		MaxSteps:      10,
		ContextBudget: 8_000,
		Trace:         func(e trace.Event) { events = append(events, e) },
		RunTool: func(_ context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "ok"}, nil
		},
	}

	s := conversation("long job", 12)
	before := len(s.Messages)

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if s.Dropped == 0 {
		t.Fatal("the run never compacted despite the reported prompt size")
	}
	if len(s.Messages) >= before {
		t.Errorf("conversation is %d messages, was %d", len(s.Messages), before)
	}
	assertWellFormed(t, s.Messages)

	// The compaction has to reach the wire, or it saved nothing.
	if len(fake.Calls) < 2 {
		t.Fatalf("only %d requests", len(fake.Calls))
	}
	if len(fake.Calls[1].Messages) >= len(fake.Calls[0].Messages) {
		t.Errorf("second request carried %d messages, first carried %d",
			len(fake.Calls[1].Messages), len(fake.Calls[0].Messages))
	}

	var compacts int
	for _, e := range events {
		if e.Kind == trace.KindCompact {
			compacts++
			// The estimate compaction acted on, not the raw reported count:
			// the conversation grew after that response, and the event should
			// say what the decision was made against.
			if e.InTokens < 80_000 {
				t.Errorf("compact event records %d input tokens, want at least the reported 80000", e.InTokens)
			}
		}
	}
	if compacts != 1 {
		t.Errorf("compact events = %d, want 1", compacts)
	}
}

// Compaction must not eat the record a resumed run reads to finish a batch.
func TestCompactLeavesPendingCallsResumable(t *testing.T) {
	s := conversation("long job", 10)
	// A batch that was requested and never finished — what a crash leaves.
	s.Messages = append(s.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "pending_1", Name: "calc", Args: json.RawMessage(`{}`)},
		},
	})

	if len(s.pendingToolCalls()) != 1 {
		t.Fatal("fixture is wrong: the call should be pending")
	}

	compact(s, 100_000, 6_000)

	pending := s.pendingToolCalls()
	if len(pending) != 1 || pending[0].ID != "pending_1" {
		t.Fatalf("compaction lost the pending call: %+v", pending)
	}
}

// The prompt size the provider reported is state, not a local variable: a
// resumed run has to make the same decision the original would have, or the
// first request after a resume is the one that blows the window.
func TestInputTokensSurvivesForResume(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 0, 0)}}
	fake.Responses[0].Usage = llm.Usage{InputTokens: 4321, OutputTokens: 7}

	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10}
	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.InputTokens != 4321 {
		t.Errorf("State.InputTokens = %d, want the provider's count", s.InputTokens)
	}
}

// If an error or interruption happens immediately after compaction (e.g.
// network failure on Complete), resume must not compact a second time against
// the already compacted conversation.
// A count recorded before compaction must not still be driving compaction
// after it.
//
// The overshoot here is deliberately mild — 9000 against a budget of 8000, so
// roughly half goes. A large overshoot cannot show this: the first pass takes
// the conversation straight down to the keep-floor, the second finds nothing
// left to drop, and the test passes whether the bug is present or not. That is
// exactly how the first version of this test was written, and it stayed green
// with the fix reverted.
//
// With a mild overshoot and three interrupted resumes the difference is plain:
// 49 -> 25 -> 9 -> 3 messages without the reset, 49 -> 25 -> 25 -> 25 with it.
func TestResumeAfterCompactionDoesNotDoubleCompact(t *testing.T) {
	fakeFail := &llm.Fake{Errs: []error{errors.New("network down")}}
	checkpointed := false
	var savedState *State
	a := &Agent{
		Provider:      fakeFail,
		Model:         "test",
		MaxSteps:      10,
		ContextBudget: 8_000,
		Checkpoint: func(st *State) error {
			checkpointed = true
			data, _ := json.Marshal(st)
			var cp State
			_ = json.Unmarshal(data, &cp)
			savedState = &cp
			return nil
		},
	}

	s := conversation("long job", 24)
	s.InputTokens = 9_000

	// First run compacts, checkpoints, and then Complete fails with network error.
	_, err := a.Run(context.Background(), s)
	if err == nil {
		t.Fatal("expected run to fail")
	}
	if !checkpointed || savedState == nil {
		t.Fatal("expected checkpoint to occur after compaction")
	}
	if savedState.Dropped == 0 {
		t.Fatal("expected compaction to have dropped messages")
	}

	droppedFirst := savedState.Dropped
	messagesFirst := len(savedState.Messages)

	// Resume, and be interrupted again. This is the leg that cascades: the
	// checkpoint still holds a pre-compaction count unless it was reset.
	a.Provider = &llm.Fake{Errs: []error{errors.New("network down again")}}
	if _, err := a.Run(context.Background(), savedState); err == nil {
		t.Fatal("expected the second run to fail too")
	}
	if savedState.Dropped != droppedFirst || len(savedState.Messages) != messagesFirst {
		t.Errorf("interrupted resume compacted again: %d messages (was %d), dropped %d (was %d)",
			len(savedState.Messages), messagesFirst, savedState.Dropped, droppedFirst)
	}

	// And a resume that succeeds carries on normally.
	a.Provider = &llm.Fake{Responses: []llm.Response{endResponse("recovered", 2000, 10)}}
	if _, err := a.Run(context.Background(), savedState); err != nil {
		t.Fatalf("resumed run failed: %v", err)
	}
	if savedState.Dropped != droppedFirst {
		t.Errorf("resumed run double-compacted: dropped %d, was %d",
			savedState.Dropped, droppedFirst)
	}
	if len(savedState.Messages) != messagesFirst+1 {
		t.Errorf("expected %d messages, got %d", messagesFirst+1, len(savedState.Messages))
	}
}

// A resumed run keeps the budget it started under, even when the resuming
// command passed no flag. Without this, `ariadne resume` silently turns
// compaction off and the first request is the one that blows the window.
//
// Asserted by whether the run actually compacts, not by which field ends up
// holding the number: the field is the mechanism, and the mechanism changed
// once already.
func TestStateContextBudgetInheritedOnResume(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 100, 10)}}
	// No budget on the agent — this is `ariadne resume` with no flag.
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10}

	s := conversation("long job", 12)
	s.ContextBudget = 5_000
	s.InputTokens = 9_000 // over the budget the checkpoint recorded
	before := len(s.Messages)

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Dropped == 0 || len(s.Messages) >= before {
		t.Errorf("resumed run did not compact: %d messages, was %d, dropped %d",
			len(s.Messages), before, s.Dropped)
	}
	assertWellFormed(t, s.Messages)
}

// An Agent outlives a Run. A budget read off one checkpoint must not still be
// in force for the next run started from the same agent — a job nobody gave a
// budget would silently begin dropping history.
//
// This is the shared-mutable-field bug this project has already paid for once,
// when a factory reading a shared run id filed every trace under the previous
// task.
func TestBudgetDoesNotLeakBetweenRuns(t *testing.T) {
	a := &Agent{
		Provider: &llm.Fake{Responses: []llm.Response{
			endResponse("one", 10, 10), endResponse("two", 10, 10),
		}},
		Model:    "test",
		MaxSteps: 10,
		// Never given a budget by anyone.
	}

	first := NewState("run_a", "first")
	first.ContextBudget = 5_000
	if _, err := a.Run(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if a.ContextBudget != 0 {
		t.Errorf("Run wrote %d back onto the agent", a.ContextBudget)
	}

	second := NewState("run_b", "second")
	if _, err := a.Run(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if second.ContextBudget != 0 {
		t.Errorf("run_b inherited run_a's budget of %d", second.ContextBudget)
	}
}

// The provider's count describes the prompt that was *sent*. Tool results land
// after that, so by the time the next request is built the conversation can be
// far larger — and compaction was reading the number from before the growth.
//
// Dogfooding produced the case: one step fetched two documents, the history
// reached 33KB, the recorded count still said 655 tokens, and compaction did
// nothing. The next request was then too large to complete, so the run was
// checkpointed, resumable, and permanently stuck.
func TestEstimateScalesWithConversationGrowth(t *testing.T) {
	s := conversation("read both documents", 1)
	s.InputTokens = 655
	s.InputChars = totalSize(s.Messages)

	// Unchanged conversation: the provider's number stands as measured.
	if got := estimatedTokens(s); got != 655 {
		t.Errorf("estimate = %d before any growth, want the measured 655", got)
	}

	// Now a step returns two large documents, as fetch would.
	s.Messages = append(s.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "big", Name: "fetch", Args: json.RawMessage(`{}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "big", Content: strings.Repeat("x", 33_000)},
		}},
	)

	est := estimatedTokens(s)
	if est <= 655 {
		t.Fatalf("estimate = %d after 33KB arrived; growth is invisible", est)
	}
	if est < 5_000 {
		t.Errorf("estimate = %d; 33KB of new content should scale it by more than that", est)
	}
	t.Logf("655 measured -> %d estimated after the documents landed", est)
}

// Without the pair there is nothing to scale against, so the measured number
// stands. A run from before this field existed must keep working.
func TestEstimateFallsBackWithoutTheCharPair(t *testing.T) {
	s := conversation("x", 3)
	s.InputTokens = 900
	s.InputChars = 0 // an older checkpoint

	if got := estimatedTokens(s); got != 900 {
		t.Errorf("estimate = %d, want the measured 900 when there is nothing to scale by", got)
	}
}

// Compaction shrinks the conversation, so the next estimate must not scale
// upward off a stale pair and immediately compact again.
func TestEstimateDoesNotGrowAfterCompaction(t *testing.T) {
	s := conversation("x", 12)
	s.InputTokens = 40_000
	s.InputChars = totalSize(s.Messages)

	if n := compact(s, estimatedTokens(s), 20_000); n == 0 {
		t.Fatal("nothing was dropped")
	}
	if got := estimatedTokens(s); got > s.InputTokens {
		t.Errorf("estimate rose to %d after compaction shrank the conversation", got)
	}
}

// In multi-turn chat, compaction must drop turn pairs and preserve role
// alternation so consecutive user roles never reach the provider.
func TestMultiTurnCompactionPreservesRoleAlternation(t *testing.T) {
	s := NewState("run_test", "turn 1: hello")
	s.Messages = append(s.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("answer 1 ", 30)}}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "turn 2: calculate 2+2"}}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("answer 2 ", 30)}}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "turn 3: calculate 3+3"}}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer 3"}}},
	)

	initialLen := len(s.Messages)
	n := compact(s, 2000, 500)
	if n == 0 {
		t.Fatal("expected compaction to drop messages")
	}
	if len(s.Messages) >= initialLen {
		t.Fatalf("messages did not shrink: got %d", len(s.Messages))
	}

	// Verify strict role alternation across all remaining messages:
	// User -> Assistant -> User -> Assistant...
	for i := 1; i < len(s.Messages); i++ {
		if s.Messages[i].Role == s.Messages[i-1].Role {
			t.Errorf("consecutive messages at %d and %d share role %s", i-1, i, s.Messages[i].Role)
		}
	}

	assertWellFormed(t, s.Messages)
}

// chat builds n question-and-answer exchanges, each labelled so a surviving
// answer can be checked against the question it belongs to.
func chat(n int) *State {
	s := NewState("run_chat", "q1")
	s.Messages = nil
	for i := 1; i <= n; i++ {
		s.Messages = append(s.Messages,
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: fmt.Sprintf("q%d", i)}}},
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: fmt.Sprintf("a%d %s", i, strings.Repeat("padding ", 20))}}},
		)
	}
	return s
}

// assertAnswersKeepTheirQuestions is the invariant role alternation does not
// imply. A compaction can leave every message alternating correctly and still
// have thrown away the question that a surviving answer is answering — which is
// what the first multi-turn implementation did, and what its test missed.
func assertAnswersKeepTheirQuestions(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant || len(m.Blocks) == 0 {
			continue
		}
		label, _, _ := strings.Cut(m.Blocks[0].Text, " ")
		if !strings.HasPrefix(label, "a") {
			continue
		}
		want := "q" + strings.TrimPrefix(label, "a")
		var prompt string
		for j := i - 1; j >= 0; j-- {
			if isUserPrompt(msgs[j]) && len(msgs[j].Blocks) > 0 {
				prompt = msgs[j].Blocks[0].Text
				break
			}
		}
		if prompt != want {
			t.Errorf("answer %s at %d follows %q, want %q", label, i, prompt, want)
		}
	}
}

// Dropping an exchange must drop the question with its answer.
//
// The opening exchange is what makes this possible. Message 0 can never go — it
// is the task — so message 1 is an answer that can never be paired with
// anything to its right without splitting some later question from its own
// answer. Protecting the pair, rather than only the task, is what lets every
// unit after it be a question and the answer to it.
func TestCompactDropsQuestionsWithTheirAnswers(t *testing.T) {
	s := chat(5)
	if n := compact(s, 4000, 500); n == 0 {
		t.Fatal("expected compaction to drop messages")
	}

	assertAnswersKeepTheirQuestions(t, s.Messages)
	assertWellFormed(t, s.Messages)
	for i := 1; i < len(s.Messages); i++ {
		if s.Messages[i].Role == s.Messages[i-1].Role {
			t.Errorf("consecutive messages at %d and %d share role %s", i-1, i, s.Messages[i].Role)
		}
	}
	if got := s.Messages[0].Blocks[0].Text; got != "q1" {
		t.Errorf("message 0 = %q, want the task", got)
	}
}

// A conversation contains no tool calls, so a digest that renders only tool
// calls renders nothing — a header announcing a list with an empty list under
// it, and no record that the questions were ever asked.
func TestDigestRecordsDroppedConversationalTurns(t *testing.T) {
	s := chat(5)
	if n := compact(s, 4000, 500); n == 0 {
		t.Fatal("expected compaction to drop messages")
	}
	if len(s.Messages[0].Blocks) < 2 {
		t.Fatal("no digest block on the task message")
	}
	digest := s.Messages[0].Blocks[1].Text

	for _, want := range []string{"q2", "a2"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest does not mention dropped turn %q:\n%s", want, digest)
		}
	}
	header, listed, ok := strings.Cut(digest, "\n")
	if !ok || strings.TrimSpace(listed) == "" {
		t.Errorf("digest is a header announcing an empty list: %q", header)
	}
}

// The head is the opening exchange only when the opening reply asked for no
// tools. A task whose first reply is a tool_use has its results one message
// later; protecting the call while its results stay droppable is the orphaned
// call this file exists to prevent.
func TestHeadNeverProtectsHalfOfAToolPair(t *testing.T) {
	tool := conversation("fetch two documents", 4)
	if got := headLen(tool.Messages); got != 1 {
		t.Errorf("headLen on a tool run = %d, want 1", got)
	}
	if n := compact(tool, 4000, 500); n == 0 {
		t.Fatal("expected compaction to drop messages")
	}
	assertWellFormed(t, tool.Messages)

	if got := headLen(chat(3).Messages); got != 2 {
		t.Errorf("headLen on a conversation = %d, want 2", got)
	}
}

// In multi-turn chat where a middle turn calls tools, compaction must never
// separate tool_use from tool_result (which leaves an orphaned result and causes
// HTTP 400), and must drop the question with its answer.
func TestCompactMultiTurnWithToolsNeverOrphansToolResults(t *testing.T) {
	s := NewState("run_test", "q1")
	s.Messages = []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q1"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a1 " + strings.Repeat("pad ", 20)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q2"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{"expr":"2+2"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "4 " + strings.Repeat("pad ", 50)},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a2 " + strings.Repeat("pad ", 20)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q3"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a3 " + strings.Repeat("pad ", 20)}}},
	}

	assertWellFormed(t, s.Messages)
	if n := compact(s, 4000, 500); n == 0 {
		t.Fatal("expected compaction to drop messages")
	}
	assertWellFormed(t, s.Messages)
	assertAnswersKeepTheirQuestions(t, s.Messages)
}

// In multi-turn chat where turn 1 called tools, compaction must protect turn 1's
// opening exchange and drop later turns completely, keeping questions with their answers.
func TestCompactMultiTurnWithToolsInTurn1PreservesQuestions(t *testing.T) {
	s := NewState("run_test", "q1")
	s.Messages = []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q1"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c0", Name: "calc", Args: json.RawMessage(`{"expr":"1+1"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c0", Content: "2 " + strings.Repeat("pad ", 50)},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a1 " + strings.Repeat("pad ", 20)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q2"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a2 " + strings.Repeat("pad ", 20)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q3"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a3 " + strings.Repeat("pad ", 20)}}},
	}

	assertWellFormed(t, s.Messages)
	if n := compact(s, 4000, 500); n == 0 {
		t.Fatal("expected compaction to drop messages")
	}
	assertWellFormed(t, s.Messages)
	assertAnswersKeepTheirQuestions(t, s.Messages)
}

// The digest's line format is written and read in this package, and this test
// is what holds the two together: the lines come from digestLines and
// mergeDigest, never from a string typed here. Change the format and this
// fails in the file that changed it, instead of every compacted eval task
// quietly failing "never called" in another package.
func TestCompactedCallsReadsWhatTheDigestWrites(t *testing.T) {
	s := NewState("run_digest", "work out 15% of 240, then write it down")

	first := []llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{"expr":"0.15*240"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "36"},
		}},
		// Prose that looks like a call must not be credited as one.
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "now try write_file(notes.txt)"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "I will call fetch(url) later"}}},
	}
	mergeDigest(&s.Messages[0], digestLines(first), len(first))

	// A second compaction rewrites the digest; calls from the first must survive it.
	second := []llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c2", Name: "write_file", Args: json.RawMessage(`{"path":"n.txt","content":"36"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c2", Content: "wrote 2 bytes"},
		}},
	}
	mergeDigest(&s.Messages[0], digestLines(second), len(first)+len(second))

	got := s.CompactedCalls()
	want := []string{"calc", "write_file"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CompactedCalls() = %v, want %v\ndigest:\n%s", got, want, s.Messages[0].Blocks[1].Text)
	}
}

func TestCompactedCallsOnAnUncompactedRun(t *testing.T) {
	if got := NewState("run_plain", "task").CompactedCalls(); got != nil {
		t.Errorf("a run with no digest reported calls: %v", got)
	}
}
