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
	// Provider is the backend that served the most recent response, where the
	// gateway reports one. Deliberately *not* folded into Model: Model is what
	// this run asks for and what a resume must keep asking for, while this is
	// what answered. A gateway can change the second without the first moving.
	//
	// Per-run because a scorecard needs one value; the trace records it per
	// response, which is the accurate account if a run is served by more than
	// one backend.
	Provider string `json:"provider,omitempty"`
	// Model is recorded on the first step and is authoritative on resume: a job
	// that finished on a different model than it started on is a different job,
	// and evals need to know which model produced a trace.
	Model string `json:"model,omitempty"`
	// Workspace is the directory this run's file tools were confined to.
	//
	// Recorded for the same reason as BaseURL: a resumed run must make the same
	// decision the original did. Resuming a run that read one repository against
	// a different directory would silently point every path it remembers at
	// files that are not the ones it saw.
	Workspace string `json:"workspace,omitempty"`
	// BaseURL is recorded so resume reaches the same provider endpoint rather
	// than silently defaulting to Gemini and failing with an incompatible model.
	BaseURL string `json:"base_url,omitempty"`
	// System is the system prompt this run started under, recorded for the same
	// reason as Model: a run that finishes under different instructions than it
	// began with is a different run, and an eval needs to know which prompt
	// produced a result.
	System string `json:"system,omitempty"`
	// Allow is the set of tools this run may call; empty means all of them.
	//
	// It lives on the state rather than only on the agent so that resume cannot
	// widen it. A run that was granted calc and fetch must still have only calc
	// and fetch an hour later, whatever flags the resuming command carries.
	Allow []string `json:"allow,omitempty"`
	// RequireApproval is recorded for the mirror-image reason: resume must not
	// be a way to drop a gate the run was started behind.
	RequireApproval []string      `json:"require_approval,omitempty"`
	Messages        []llm.Message `json:"messages"`
	Steps           int           `json:"steps"`
	Cost            float64       `json:"cost_usd"`
	// Memory records that this run was started with MEMORY.md enabled.
	//
	// The loop never reads it — it is the command's bookkeeping, kept here for
	// the same reason as BaseURL: a resumed run has to make the same decision
	// the original did. Inferring it instead, from the approval list containing
	// "remember" or from the system prompt containing a fence marker, works
	// until either of those is reworded and then fails silently.
	Memory bool `json:"memory,omitempty"`
	// ContextBudget is the prompt-token ceiling this run aims to stay under.
	// Saved on State so a resumed run inherits the budget rather than silently
	// disabling compaction.
	ContextBudget int `json:"context_budget,omitempty"`
	// InputTokens is the prompt size the provider reported for the last
	// request. Recorded rather than recomputed because compaction is driven by
	// it, and a resumed run has to make the same decision the original would
	// have — otherwise the first request after a resume is the one that blows
	// the context window.
	InputTokens int `json:"input_tokens,omitempty"`
	// InputChars is how big the conversation was when InputTokens was measured.
	//
	// The pair is what makes the token count usable. InputTokens describes the
	// prompt that was *sent*; by the time the next request is built, tools have
	// returned and the conversation can be much larger. Dogfooding found the
	// case that matters: a step that fetched two documents grew the history to
	// 33KB while the recorded count still said 655 tokens, so compaction looked
	// at a number from before the growth and did nothing — in the one situation
	// where it was the only thing that could have helped.
	InputChars int `json:"input_chars,omitempty"`
	// Dropped counts messages compaction has removed over the life of the run.
	// The conversation no longer says how long it was; this does, and the trace
	// still holds every message that ever existed.
	Dropped int `json:"dropped,omitempty"`
}

// allows reports whether name may be called in this run.
//
// Empty means everything, which is the permissive default a `--allow` flag
// narrows. Expressing "no tools at all" is the job of giving the agent no
// tools, not of an empty list — an empty slice does not survive JSON anyway.
func (s *State) allows(name string) bool {
	if len(s.Allow) == 0 {
		return true
	}
	return contains(s.Allow, name)
}

// needsApproval reports whether name must be approved before each call.
func (s *State) needsApproval(name string) bool {
	return contains(s.RequireApproval, name)
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
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

// HasPendingToolCalls reports whether a batch was left unfinished — a crash or
// a cancelled turn. A single-shot command never needs this: Run finishes any
// pending batch on its first call, unconditionally. A REPL calls Run more than
// once, so it needs to ask first — adding a message on top of an unfinished
// batch is two failures at once (see AddUserMessage), and finishing a batch
// that was never interrupted would re-ask the model with nothing new to say.
func (s *State) HasPendingToolCalls() bool {
	return len(s.pendingToolCalls()) > 0
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
