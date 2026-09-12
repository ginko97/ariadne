package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// scripted builds an AgentFactory whose provider replays canned responses —
// the whole eval loop, end to end, with no network and no key.
func scripted(responses ...llm.Response) AgentFactory {
	return func(model, runID string, maxSteps int) *loop.Agent {
		return &loop.Agent{
			Provider: &llm.Fake{Responses: responses},
			Model:    model,
			MaxSteps: maxSteps,
			RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
				return llm.ToolResult{Content: "36"}, nil
			},
		}
	}
}

func toolThenAnswer(text string) []llm.Response {
	return []llm.Response{
		{
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "calc",
				Args: json.RawMessage(`{"expr":"240*0.15"}`)}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 50, OutputTokens: 20, Cost: 0.0001},
		},
		{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: text}},
			Stop:   llm.StopEnd,
			Usage:  llm.Usage{InputTokens: 90, OutputTokens: 10, Cost: 0.0002},
		},
	}
}

func ids(model, taskID string) string { return "run_" + taskID }

func TestRunTasksScoresEach(t *testing.T) {
	tasks := []Task{
		{ID: "a", Prompt: "p", Expect: "36", MustCall: []string{"calc"}, MaxSteps: 5},
		{ID: "b", Prompt: "p", Expect: "99", MustCall: []string{"calc"}, MaxSteps: 5},
	}

	// Each task gets a fresh agent, so both see the same script.
	results := RunTasks(context.Background(), tasks, "test/model",
		scripted(toolThenAnswer("the answer is **36**")...), ids)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if !results[0].Pass {
		t.Errorf("task a should pass: %s", results[0].Reason)
	}
	if results[1].Pass {
		t.Error("task b expected 99 and should fail")
	}

	// Cost is summed from what the provider reported, per run.
	if results[0].Cost < 0.00029 || results[0].Cost > 0.00031 {
		t.Errorf("cost = %v, want ~0.0003", results[0].Cost)
	}
	if results[0].RunID != "run_a" {
		t.Errorf("RunID = %q", results[0].RunID)
	}
}

// One bad task must not discard the rest of the sweep.
func TestRunTasksContinuesAfterFailure(t *testing.T) {
	// Only two responses are scripted, so the second task's agent runs dry and
	// its run fails — exactly like a provider hiccup mid-sweep.
	tasks := []Task{
		{ID: "a", Prompt: "p", Expect: "36", MaxSteps: 5},
		{ID: "b", Prompt: "p", Expect: "36", MaxSteps: 5},
	}

	exhausted := func(model, runID string, maxSteps int) *loop.Agent {
		return &loop.Agent{
			Provider: &llm.Fake{}, // no responses at all
			Model:    model, MaxSteps: maxSteps,
		}
	}

	results := RunTasks(context.Background(), tasks, "m", exhausted, ids)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 — the sweep stopped early", len(results))
	}
	for _, r := range results {
		if r.Pass {
			t.Errorf("%s scored a pass despite the run failing", r.TaskID)
		}
		if !strings.Contains(r.Reason, "run failed") {
			t.Errorf("%s reason = %q", r.TaskID, r.Reason)
		}
	}
}

// Cancellation stops the sweep but keeps what was already scored.
func TestRunTasksStopsOnCancel(t *testing.T) {
	tasks := []Task{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := RunTasks(ctx, tasks, "m", scripted(), ids); len(got) != 0 {
		t.Fatalf("got %d results, want 0 — nothing should have run", len(got))
	}
}

func TestScorecardTable(t *testing.T) {
	sc := NewScorecard("some/model", "abc123", []Result{
		{TaskID: "a", Pass: true, Cost: 0.001},
		{TaskID: "b", Pass: false, Cost: 0.002, Reason: "never called: calc"},
	})

	var b strings.Builder
	sc.WriteTable(&b)
	out := b.String()

	for _, want := range []string{"some/model", "1/2", "FAIL", "b", "never called: calc"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}
