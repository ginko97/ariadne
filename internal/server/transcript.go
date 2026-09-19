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
	Kind    string `json:"kind"` // "prompt", "answer", "tool_call", "tool_result"
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
				out = append(out, transcriptEntry{Kind: kind, Text: b.Text})

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
