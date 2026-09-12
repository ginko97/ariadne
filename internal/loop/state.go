package loop

import "github.com/ginko97/ariadne/internal/llm"

// State is everything about one run.
//
// This struct is what gets written to disk after every tool call, and what
// `ariadne resume` loads back. That is why it holds no interfaces, no channels,
// and no funcs — every field must survive a JSON round-trip unchanged.
//
// The schema version lives on the checkpoint envelope, not here: it describes
// the file format, not the run.
type State struct {
	RunID string `json:"run_id"`
	Task  string `json:"task"`
	// Model is recorded on the first step and is authoritative on resume: a job
	// that finished on a different model than it started on is a different job,
	// and evals need to know which model produced a trace.
	Model string `json:"model,omitempty"`
	// BaseURL is recorded so resume reaches the same provider endpoint rather
	// than silently defaulting to Gemini and failing with an incompatible model.
	BaseURL string `json:"base_url,omitempty"`
	// System is the system prompt this run started under, recorded for the same
	// reason as Model: a run that finishes under different instructions than it
	// began with is a different run, and an eval needs to know which prompt
	// produced a result.
	System   string        `json:"system,omitempty"`
	Messages []llm.Message `json:"messages"`
	Steps    int           `json:"steps"`
	Cost     float64       `json:"cost_usd"`
}

// NewState seeds a fresh run with the user's task.
//
// Resume does not call this — it unmarshals a State from disk and hands it
// straight to Run, which is why Run takes *State rather than a task string.
func NewState(runID, task string) *State {
	return &State{
		RunID: runID,
		Task:  task,
		Messages: []llm.Message{{
			Role:   llm.RoleUser,
			Blocks: []llm.Block{{Type: llm.BlockText, Text: task}},
		}},
	}
}

// pendingToolCalls returns tool calls from the final assistant turn that do not
// yet have a result.
//
// This is what makes resume safe. Asking the model again would produce fresh,
// provider-assigned call IDs, so a tool that already ran would run a second time
// under a different key — an idempotency key only helps when the retry carries
// the same key. Finishing the batch from the conversation itself keeps the
// original IDs and never re-fires a completed call.
func (s *State) pendingToolCalls() []llm.ToolCall {
	if len(s.Messages) == 0 {
		return nil
	}
	last := s.Messages[len(s.Messages)-1]

	var assistant llm.Message
	done := map[string]bool{}

	switch {
	case last.Role == llm.RoleAssistant:
		// Crashed between requesting the calls and opening the results message.
		assistant = last
	case isToolResults(last) && len(s.Messages) >= 2:
		// Crashed part-way through the batch.
		assistant = s.Messages[len(s.Messages)-2]
		for _, b := range last.Blocks {
			if b.Type == llm.BlockToolResult {
				done[b.CallID] = true
			}
		}
	default:
		return nil
	}

	if assistant.Role != llm.RoleAssistant {
		return nil
	}

	var pending []llm.ToolCall
	for _, b := range assistant.Blocks {
		if b.Type != llm.BlockToolUse || done[b.ID] {
			continue
		}
		pending = append(pending, llm.ToolCall{ID: b.ID, Name: b.Name, Args: b.Args})
	}
	return pending
}

// isToolResults reports whether m is a results message: a user turn carrying
// only tool_result blocks. A user turn with no blocks counts — that is the
// moment between opening a batch and the first result landing. The initial task
// message does not, because it carries text.
func isToolResults(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type != llm.BlockToolResult {
			return false
		}
	}
	return true
}
