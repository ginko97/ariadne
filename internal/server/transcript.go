package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// transcriptResponse is a conversation as something to read.
//
// Not the checkpoint. A checkpoint is working state for the loop — tool call
// ids, raw argument JSON, the fields resume pairs on — and none of that is what
// a person scrolling back wants to see. Sending it because it happens to be
// marshalable would make the wire format follow the loop's internals, and every
// later change to State would become a change to the page.
type transcriptResponse struct {
	RunID string  `json:"run_id"`
	Title string  `json:"title"`
	Model string  `json:"model"`
	Turns int     `json:"turns"`
	Steps int     `json:"steps"`
	Cost  float64 `json:"cost_usd"`
	// CostUnknown: some step's cost was not measured, so Cost is a lower
	// bound, or no figure at all when it is zero.
	CostUnknown bool `json:"cost_unknown,omitempty"`
	// Workspace is the folder the conversation works in, shown so the
	// person can always see where it can read and write.
	Workspace string            `json:"workspace,omitempty"`
	Pending   bool              `json:"pending,omitempty"`
	Messages  []transcriptEntry `json:"messages"`
}

// transcriptEntry is one thing that happened, in order.
//
// Kind rather than role, because "user" covers two different events: a person
// asking something, and the loop handing back tool results. Collapsing them
// would render a wall of JSON as though the person had typed it.
type transcriptEntry struct {
	Kind string `json:"kind"` // "prompt", "answer", "tool_call", "tool_result"
	// Model is set on an answer: the model that wrote it, which may not be
	// the one the conversation is on now.
	Model   string `json:"model,omitempty"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// handleTranscript returns one conversation's messages.
//
// The list endpoint says which conversations exist; this says what is in one.
// Without it a browser can resume a conversation it cannot display — the run id
// is enough for the agent, which holds the history in its checkpoint, and not
// enough for the person, who cannot see it.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !sanitiseRunID(runID) {
		httpError(w, http.StatusBadRequest, "malformed run_id")
		return
	}

	state, err := s.Store.Load(runID)
	if err != nil {
		if errors.Is(err, loop.ErrNoCheckpoint) {
			httpError(w, http.StatusNotFound, "no such conversation")
			return
		}
		httpError(w, http.StatusInternalServerError, "failed to load conversation")
		return
	}

	ws := state.Workspace
	if ws == "" {
		ws = s.DefaultWorkspace
	}

	out := transcriptResponse{
		RunID:       state.RunID,
		Title:       state.Task,
		Model:       state.Model,
		Turns:       state.Turns(),
		Steps:       state.Steps,
		Cost:        state.Cost,
		CostUnknown: state.UnpricedSteps > 0,
		Workspace:   ws,
		Pending:     state.HasPendingToolCalls(),
		Messages:    entries(state.Messages),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// compactJSON undoes the indentation a checkpoint round trip adds.
//
// Store.Save marshals with MarshalIndent, which re-indents embedded
// json.RawMessage — so arguments written as {"expr":"21*3"} come back with
// newlines and fourteen spaces inside them. Semantically identical, and on a
// page it reads as though the model emitted broken formatting.
//
// Damaged arguments are left exactly as they are. After the malformed-tool-call
// fix they are preserved as a JSON string rather than discarded, and showing
// what actually arrived is the point of showing them at all.
func compactJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// entries flattens the conversation into what happened, in order.
//
// A message can hold several blocks — an assistant turn often carries prose and
// a tool call together — and each is its own thing to read, so one message can
// become several entries. Empty text is dropped rather than rendered as a blank
// bubble: a tool-only assistant turn has no prose, and showing an empty one
// would suggest the model said nothing when it in fact did something.
func entries(msgs []llm.Message) []transcriptEntry {
	out := make([]transcriptEntry, 0, len(msgs))
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				kind := "answer"
				if m.Role == llm.RoleUser {
					kind = "prompt"
					if strings.HasPrefix(b.Text, "[") && strings.Contains(b.Text, "earlier messages have been dropped") {
						kind = "notice"
					}
				}
				entry := transcriptEntry{Kind: kind, Text: b.Text}
				if kind == "answer" {
					entry.Model = m.Model
				}
				out = append(out, entry)

			case llm.BlockToolUse:
				out = append(out, transcriptEntry{
					Kind: "tool_call", Tool: b.Name, Args: compactJSON(b.Args),
				})

			case llm.BlockToolResult:
				out = append(out, transcriptEntry{
					Kind: "tool_result", Text: b.Content, IsError: b.IsError,
				})
			}
		}
	}
	return out
}

// answeredBy is the model that produced the conversation's last answer, or
// empty when nothing has answered yet.
//
// Read backwards from the end rather than taken from state.Model, because a
// turn is the last thing that happened before a model switch as often as it is
// the first thing after one.
func answeredBy(state *loop.State) string {
	for i := len(state.Messages) - 1; i >= 0; i-- {
		if state.Messages[i].Role == llm.RoleAssistant {
			return state.Messages[i].Model
		}
	}
	return ""
}

// handleDelete removes a conversation and everything it wrote: checkpoint and
// trace both.
//
// Irreversible, so it takes the claim first. A run in the middle of a turn is
// refused with 409 — the same refusal a second turn gets — rather than having
// its directory pulled out from under a loop that is still writing
// checkpoints into it.
//
// The claim is held for the delete and released after, which is why this is not
// simply Store.Delete behind a route: the store knows nothing about which runs
// a server is currently running.
//
// Mutating, so guard already required the CSRF token: without it any page you
// visit could delete your conversations by their ids, and ids appear in the
// listing this same server hands out.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !sanitiseRunID(runID) {
		httpError(w, http.StatusBadRequest, "malformed run_id")
		return
	}

	release, ok := s.claim(runID)
	if !ok {
		httpError(w, http.StatusConflict, "that conversation is busy; stop the turn first")
		return
	}
	defer release()

	if _, err := s.Store.LoadCheckpoint(runID); err != nil {
		if errors.Is(err, loop.ErrNoCheckpoint) {
			httpError(w, http.StatusNotFound, "no such conversation")
			return
		}
		// An unreadable checkpoint is still a conversation to delete — that is
		// arguably the one you most want gone — so this falls through rather
		// than refusing.
	}

	if err := s.Store.Delete(runID); err != nil {
		httpError(w, http.StatusInternalServerError, "failed to delete conversation")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": runID})
}
