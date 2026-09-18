package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
)

// approvalTimeout bounds how long a turn waits for somebody to answer.
//
// A wait with nobody there is a denial, not a hang: the run holds a claim on the
// conversation while it waits, so an unanswered prompt would wedge that
// conversation until the process stopped. Long enough to walk back to the
// keyboard, short enough that a forgotten tab does not hold a run all day.
const approvalTimeout = 5 * time.Minute

// approvals lets a second request answer a question the first one is blocked on.
//
// The turn runs inside handleChat with its SSE stream open, and Agent.Approve is
// called synchronously inside the loop — so the prompt goes out on the stream
// that is already open, and the answer arrives on POST /api/approve, which has
// no way to reach the handler except through here.
//
// Keyed by run and call id together. The call id is provider-assigned and unique
// within a turn; pairing it with the run means a decision cannot be applied to
// somebody else's conversation by guessing a call id.
type approvals struct {
	mu      sync.Mutex
	waiting map[string]chan bool
}

func newApprovals() *approvals { return &approvals{waiting: map[string]chan bool{}} }

func approvalKey(runID, callID string) string { return runID + "\x00" + callID }

// wait registers a question and returns the channel its answer will arrive on.
//
// Buffered, so decide never blocks on a waiter that has already given up — a
// timeout or a closed tab leaves nobody reading, and an unbuffered send there
// would block the approving request forever.
func (a *approvals) wait(key string) chan bool {
	ch := make(chan bool, 1)
	a.mu.Lock()
	a.waiting[key] = ch
	a.mu.Unlock()
	return ch
}

func (a *approvals) forget(key string) {
	a.mu.Lock()
	delete(a.waiting, key)
	a.mu.Unlock()
}

// decide answers a waiting question, reporting whether one was waiting.
//
// A decision for something nobody asked is not silently accepted: it means the
// turn already timed out, the tab was closed, or the id was wrong, and telling
// the caller is the difference between "denied" and "your answer went nowhere".
func (a *approvals) decide(key string, approve bool) bool {
	a.mu.Lock()
	ch, ok := a.waiting[key]
	if ok {
		delete(a.waiting, key)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	ch <- approve
	return true
}

// approver is the Agent.Approve closure for one streaming turn.
//
// Every path that is not an explicit yes denies, which is the rule the terminal
// approver already follows and the reason it is written as a switch on the
// reasons rather than a happy path with error handling: a closed tab, a person
// who never answers, and a server that restarted are all "no", and none of them
// should be distinguishable from "no" by the tool that was asked for.
func (s *Server) approver(runID string, out *sseWriter) func(context.Context, llm.ToolCall) (bool, error) {
	return func(ctx context.Context, c llm.ToolCall) (bool, error) {
		key := approvalKey(runID, c.ID)
		ch := s.approvals.wait(key)
		defer s.approvals.forget(key)

		out.event("approval_required", map[string]any{
			"run_id":  runID,
			"call_id": c.ID,
			"tool":    c.Name,
			"args":    compactJSON(c.Args),
		})

		select {
		case ok := <-ch:
			return ok, nil
		case <-ctx.Done():
			// The tab closed or the request was cancelled. Nobody is reading the
			// prompt that was just sent, so nobody is going to answer it.
			return false, ctx.Err()
		case <-time.After(approvalTimeout):
			out.event("approval_timeout", map[string]any{"call_id": c.ID, "tool": c.Name})
			return false, nil
		}
	}
}

type approveRequest struct {
	RunID   string `json:"run_id"`
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
}

// handleApprove records a decision for a call some turn is blocked on.
//
// Mutating, so the guard requires the CSRF token on it exactly as it does for
// /api/chat — and here the stakes are the ones that note was written about: the
// worst case is not a denial of service, it is a page approving a tool call on
// somebody's behalf.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if !sanitiseRunID(req.RunID) || req.CallID == "" {
		httpError(w, http.StatusBadRequest, "run_id and call_id are required")
		return
	}

	if !s.approvals.decide(approvalKey(req.RunID, req.CallID), req.Approve) {
		// Gone rather than not-found: the question existed and no longer does,
		// which is what a timeout or a closed tab leaves behind. A client that
		// answered too late should be told that, not told it asked wrongly.
		httpError(w, http.StatusGone, "nothing is waiting on that call; it timed out or was already answered")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "approved": req.Approve})
}
