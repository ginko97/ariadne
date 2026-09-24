package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ginko97/ariadne/internal/cite"
	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

type chatRequest struct {
	RunID   string `json:"run_id"` // empty starts a new conversation
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	// Workspace is the folder a new conversation works in. Only honoured
	// when RunID is empty: a conversation's folder is fixed when it starts,
	// for the reason resume keeps its checkpoint's — every path it has seen
	// points into that folder.
	Workspace string `json:"workspace,omitempty"`
	// Resume finishes an interrupted turn (a batch with pending tool calls)
	// without appending a new message.
	Resume bool `json:"resume,omitempty"`
	// Retry asks the model again for a turn whose answer never arrived — the
	// connection dropped, the provider failed, or Stop came mid-answer —
	// without appending a message. Only for a conversation that awaits an
	// answer (loop.State.AwaitsAnswer).
	Retry bool `json:"retry,omitempty"`
	// Brief is the relative or workspace path to a .md brief to seed a new conversation.
	Brief string `json:"brief,omitempty"`
	// BriefSHA256 is the digest POST /api/brief returned for the text the
	// page showed. Required with Brief, and it must match the file as it is
	// read now: Start is consent to the text on the screen, not to whatever
	// the file holds by the time the request arrives.
	BriefSHA256 string `json:"brief_sha256,omitempty"`
}

// handleChat runs one turn and streams it.
//
// Everything that can refuse the request happens before a single SSE byte is
// written, because once the stream is open the status code is already sent and
// a failure can only be reported as an event. So: parse, claim, load, and only
// then switch to text/event-stream.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		httpError(w, http.StatusServiceUnavailable, "no provider is set up yet: choose one in Settings")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	req.Brief = strings.TrimSpace(req.Brief)
	if !req.Resume && !req.Retry && req.Message == "" && req.Brief == "" {
		httpError(w, http.StatusBadRequest, "a message or a task file is required")
		return
	}
	if req.Retry && (req.RunID == "" || req.Message != "" || req.Brief != "" || req.Resume) {
		httpError(w, http.StatusBadRequest, "try again takes a conversation and nothing else: no message, task file or resume")
		return
	}
	if req.Resume && req.Brief != "" {
		httpError(w, http.StatusBadRequest, "cannot run a task file when resuming an interrupted turn")
		return
	}
	if req.Resume && req.RunID == "" {
		httpError(w, http.StatusBadRequest, "run_id is required to resume")
		return
	}
	if req.Resume && req.Message != "" {
		httpError(w, http.StatusBadRequest, "cannot send a message when resuming an interrupted turn")
		return
	}
	if req.RunID != "" && req.Brief != "" {
		httpError(w, http.StatusBadRequest, "a task file starts a new conversation; it cannot be added to one")
		return
	}
	if req.Brief != "" && req.Message != "" {
		httpError(w, http.StatusBadRequest, "cannot send both a task file and a message")
		return
	}
	if req.Brief != "" && req.BriefSHA256 == "" {
		httpError(w, http.StatusBadRequest, "a task file runs only after it has been shown: brief_sha256 is required")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Workspace = strings.TrimSpace(req.Workspace)
	if req.RunID != "" && !sanitiseRunID(req.RunID) {
		httpError(w, http.StatusBadRequest, "malformed run_id")
		return
	}

	// A new conversation gets its id before the claim, so the claim covers the
	// whole turn including the one that creates the run.
	runID := req.RunID
	fresh := runID == ""

	var workspace string
	if fresh && req.Workspace != "" {
		ws, err := checkWorkspace(req.Workspace)
		if err != nil {
			httpError(w, http.StatusBadRequest, "folder: "+err.Error())
			return
		}
		workspace = ws
	} else if def := s.DefaultFolder(); fresh && def != "" {
		workspace = def
	}

	var briefContent string
	var briefRelPath string
	if fresh && req.Brief != "" {
		bws := workspace
		if bws == "" {
			if cwd, err := os.Getwd(); err == nil {
				bws = cwd
			}
		}
		rel, content, err := readWorkspaceBrief(bws, req.Brief)
		if err != nil {
			httpError(w, http.StatusBadRequest, "task file: "+err.Error())
			return
		}
		if briefDigest(content) != req.BriefSHA256 {
			httpError(w, http.StatusConflict, "task file "+rel+" changed since it was shown; show it again before running it")
			return
		}
		briefRelPath = rel
		briefContent = content
		req.Message = content
		if workspace == "" {
			workspace = bws
		}
	}
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
		state.Workspace = workspace
		state.Memory = s.MemoryStore.Path != ""
		if briefRelPath != "" {
			state.Brief = briefRelPath
		}
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
		// Refused rather than ignored: a page that sent a folder believes
		// the conversation will use it, and silently working somewhere else
		// is the worse outcome.
		if req.Workspace != "" && filepath.Clean(req.Workspace) != filepath.Clean(state.Workspace) {
			httpError(w, http.StatusBadRequest,
				"a conversation's folder cannot change; start a new conversation for another folder")
			return
		}
		if req.Retry {
			// Checked here rather than left to the loop so the refusal is a
			// status code, not an event on a stream that already said 200.
			if !state.AwaitsAnswer() {
				httpError(w, http.StatusBadRequest,
					"nothing to try again: this conversation is not waiting for an answer")
				return
			}
		} else if req.Resume {
			if !state.HasPendingToolCalls() {
				httpError(w, http.StatusBadRequest,
					"this conversation has no unfinished tool call to resume")
				return
			}
			if req.Model != "" && req.Model != state.Model {
				httpError(w, http.StatusBadRequest,
					"cannot switch model while resuming an unfinished tool call")
				return
			}
		} else if state.HasPendingToolCalls() {
			// An unfinished batch cannot take a new message: AddUserMessage
			// refuses, and it is right to. Answered rather than flushed silently,
			// because flushing here would stream the answer to whatever was asked
			// before the interruption — not to what was just sent.
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
	// The folder goes with it: the page cannot work it out once the default
	// can change in settings while a conversation is open, and it shows the
	// folder and lists task files from it.
	out.event("start", map[string]any{"run_id": runID, "workspace": state.Workspace})
	if briefRelPath != "" {
		out.event("brief", map[string]any{"path": briefRelPath, "content": briefContent})
	}

	agent, cleanup := s.NewAgent(runID, state, deltaEvents(out))
	if cleanup != nil {
		defer cleanup()
	}
	// Set after construction rather than through the factory: the approver needs
	// this request's stream, which the factory has no way to know about, and
	// Agent.Approve being a plain func field is exactly why no interface was
	// built for it.
	isBrief := state.Brief != ""
	agent.ApproveBatch = s.batchApprover(runID, out, state.Workspace, isBrief)
	agent.Approve = s.approver(runID, out, state.Workspace, isBrief)

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
	if fresh || req.Resume {
		answer, err = agent.Run(ctx, state)
	} else if req.Retry {
		answer, err = agent.Retry(ctx, state)
	} else {
		answer, err = agent.ChatTurn(ctx, state, req.Message)
	}
	if err != nil {
		if errors.Is(err, errApprovalWaiting) {
			out.event("waiting", map[string]any{"run_id": runID, "brief": state.Brief})
			return
		}
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
		// The page shows "cost unknown" instead of a figure when set.
		"cost_unknown": state.UnpricedSteps > 0,
		// The model the conversation is on, for the picker...
		"model": state.Model,
		// ...and the one that wrote this answer, which the page puts under it.
		// They differ when a provider serves something other than what was
		// asked for, and after a switch, when the picker is already ahead.
		"answered_by": answeredBy(state),
		// URLs this turn cited that nothing in the conversation opened. See
		// internal/cite: a lookup in the checkpoint, not a judgement.
		"unopened": cite.Unopened(state.Messages),
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
		if c.Stop != "" || c.Usage.Reported() {
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
