package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

type chatRequest struct {
	RunID   string `json:"run_id"` // empty starts a new conversation
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
}

// handleChat runs one turn and streams it.
//
// Everything that can refuse the request happens before a single SSE byte is
// written, because once the stream is open the status code is already sent and
// a failure can only be reported as an event. So: parse, claim, load, and only
// then switch to text/event-stream.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		httpError(w, http.StatusBadRequest, "message is required")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.RunID != "" && !sanitiseRunID(req.RunID) {
		httpError(w, http.StatusBadRequest, "malformed run_id")
		return
	}

	// A new conversation gets its id before the claim, so the claim covers the
	// whole turn including the one that creates the run.
	runID := req.RunID
	fresh := runID == ""
	if fresh {
		runID = s.NewRunID()
	}

	release, ok := s.claim(runID)
	if !ok {
		httpError(w, http.StatusConflict, "a turn is already running for this conversation")
		return
	}
	defer release()

	var state *loop.State
	if fresh {
		// NewState seeds messages[0] from the task, so the first message is
		// the task and the turn is a plain Run — the same shape cmdRun and
		// cmdChat's first turn use. Adding it afterwards would leave an empty
		// message at index 0 that compaction can never drop.
		state = loop.NewState(runID, req.Message)
	} else {
		st, err := s.Store.Load(runID)
		if err != nil {
			if errors.Is(err, loop.ErrNoCheckpoint) {
				httpError(w, http.StatusNotFound, "no such conversation")
				return
			}
			httpError(w, http.StatusInternalServerError, "failed to load conversation")
			return
		}
		state = st
		// An unfinished batch cannot take a new message: AddUserMessage
		// refuses, and it is right to. Answered rather than flushed silently,
		// because flushing here would stream the answer to whatever was asked
		// before the interruption — not to what was just sent. `ariadne chat
		// <run-id>` finishes it; a resume endpoint is the eventual home.
		if state.HasPendingToolCalls() {
			httpError(w, http.StatusConflict,
				"this conversation has an unfinished tool call; resume it before sending a new message")
			return
		}
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	out := &sseWriter{w: w, f: flusher}
	out.event("start", map[string]any{"run_id": runID})

	agent, cleanup := s.NewAgent(runID, state, deltaEvents(out))
	if cleanup != nil {
		defer cleanup()
	}
	// Set after construction rather than through the factory: the approver needs
	// this request's stream, which the factory has no way to know about, and
	// Agent.Approve being a plain func field is exactly why no interface was
	// built for it.
	agent.Approve = s.approver(runID, out)

	if req.Model != "" {
		agent.Model = req.Model
	} else if !fresh && state.Model != "" {
		// A conversation resumed without an explicit model switch keeps the
		// model it was already using, rather than silently adopting the
		// server's startup model.
		agent.Model = state.Model
	}

	// r.Context() on purpose: a closed tab cancels the turn, the loop's guards
	// see a cancelled context, and per-call checkpointing means what already
	// ran is on disk. A disconnect costs the rest of the turn, never the work.
	ctx := r.Context()

	var answer string
	var err error
	if fresh {
		answer, err = agent.Run(ctx, state)
	} else {
		answer, err = agent.ChatTurn(ctx, state, req.Message)
	}
	if err != nil {
		// The status line went out with the headers, so a failure is an event.
		// The run id goes with it: the turn failed, the conversation did not,
		// and resuming it is the whole point of having checkpointed it.
		out.event("error", map[string]any{"run_id": runID, "error": err.Error()})
		return
	}

	out.event("done", map[string]any{
		"run_id": runID,
		"answer": answer,
		"turns":  state.Turns(),
		"steps":  state.Steps,
		"cost":   state.Cost,
		"model":  state.Model,
	})
}

// deltaEvents turns provider chunks into SSE events.
//
// Same rules printDelta follows on the terminal, and for the same reasons:
// announce a tool once rather than once per fragment, and never emit argument
// fragments, which are not valid JSON on their own and cannot be assembled by
// the receiver anyway. Identity is the index — the id and name arrive only on
// the first fragment — and indices restart at 0 each turn, so the set is
// cleared when the turn stops.
func deltaEvents(out *sseWriter) func(llm.Chunk) {
	announced := map[int]bool{}
	return func(c llm.Chunk) {
		if c.Text != "" {
			out.event("token", map[string]any{"text": c.Text})
		}
		if d := c.ToolCall; d != nil && d.Name != "" && !announced[d.Index] {
			announced[d.Index] = true
			out.event("tool", map[string]any{"name": d.Name})
		}
		if c.Stop != "" || c.Usage.InputTokens > 0 || c.Usage.OutputTokens > 0 || c.Usage.Cost > 0 {
			clear(announced)
		}
	}
}

// sseWriter writes one event per call and flushes it.
//
// The flush is the feature. Without it the response sits in a buffer until the
// handler returns, at which point "streaming" has delivered the whole answer at
// once — which is what the CLI already does, more simply.
//
// A write error means the receiver is gone. It is recorded rather than
// returned, because nothing upstream can act on it: the delta callback is
// synchronous inside the loop and stopping the turn is the context's job, not
// the printer's.
type sseWriter struct {
	w   http.ResponseWriter
	f   http.Flusher
	err error
}

func (e *sseWriter) event(name string, v any) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		e.err = err
		return
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		e.err = err
		return
	}
	e.f.Flush()
}
