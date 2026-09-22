package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/tool"
)

// defaultApprovalTimeout bounds how long a turn waits for somebody to answer.
//
// A wait with nobody there is a denial, not a hang: the run holds a claim on the
// conversation while it waits, so an unanswered prompt would wedge that
// conversation until the process stopped. Long enough to walk back to the
// keyboard, short enough that a forgotten tab does not hold a run all day.
//
// Per Server rather than a package variable, so a test can shorten it for one
// server without every other request in the package, or a later phase of the
// same test, racing a card against it.
const defaultApprovalTimeout = 5 * time.Minute

// errApprovalWaiting is returned when an approval card for a brief times out.
// Instead of treating the lack of immediate answer as a denial, the run yields
// its claim and stays pending until the operator reviews it.
var errApprovalWaiting = errors.New("approval card is waiting for operator review")

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
	waiting map[string]*pendingBatch
}

// pendingBatch is one card waiting for an answer, with what it offered — so an
// answer can be checked against the question before it is accepted.
type pendingBatch struct {
	ch    chan answer
	calls map[string]bool // call IDs on the card
	keys  map[string]bool // destinations the card offered to allow
}

// answer is the operator's reply to one card.
type answer struct {
	approve bool
	deny    []string // call IDs to skip while approving the rest
	grant   []string // destinations to allow for the rest of the turn
}

func newApprovals() *approvals { return &approvals{waiting: map[string]*pendingBatch{}} }

func approvalKey(runID, callID string) string { return runID + "\x00" + callID }

// wait registers a question and returns the channel its answer will arrive on.
//
// Buffered, so decide never blocks on a waiter that has already given up — a
// timeout or a closed tab leaves nobody reading, and an unbuffered send there
// would block the approving request forever.
func (a *approvals) wait(key string, calls, keys []string) chan answer {
	p := &pendingBatch{ch: make(chan answer, 1), calls: map[string]bool{}, keys: map[string]bool{}}
	for _, c := range calls {
		p.calls[c] = true
	}
	for _, k := range keys {
		if k != "" {
			p.keys[k] = true
		}
	}
	a.mu.Lock()
	a.waiting[key] = p
	a.mu.Unlock()
	return p.ch
}

func (a *approvals) forget(key string) {
	a.mu.Lock()
	delete(a.waiting, key)
	a.mu.Unlock()
}

// isWaiting reports whether any card for runID is actively waiting for an answer.
func (a *approvals) isWaiting(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	prefix := runID + "\x00"
	for k := range a.waiting {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// decide answers a waiting question with a plain yes or no for every call on
// it, reporting whether one was waiting.
//
// A decision for something nobody asked is not silently accepted: it means the
// turn already timed out, the tab was closed, or the id was wrong, and telling
// the caller is the difference between "denied" and "your answer went nowhere".
func (a *approvals) decide(key string, approve bool) bool {
	found, _ := a.decideBatch(key, answer{approve: approve})
	return found
}

// errOffCard is an answer that names a call or a destination the card did not
// show. It is refused before anything is delivered, so the page can correct it
// and the question stays open.
var errOffCard = errors.New("that answer names something this card did not offer")

// decideBatch delivers a full answer, after checking it only refers to what
// the card showed. The page cannot allow a destination nobody was shown, or
// skip a call that was never asked about.
func (a *approvals) decideBatch(key string, ans answer) (found bool, err error) {
	a.mu.Lock()
	p, ok := a.waiting[key]
	if !ok {
		a.mu.Unlock()
		return false, nil
	}
	for _, id := range ans.deny {
		if !p.calls[id] {
			a.mu.Unlock()
			return true, errOffCard
		}
	}
	for _, k := range ans.grant {
		if !p.keys[k] {
			a.mu.Unlock()
			return true, errOffCard
		}
	}
	delete(a.waiting, key)
	a.mu.Unlock()
	p.ch <- ans
	return true, nil
}

// cardCall is one call as the page draws it.
type cardCall struct {
	CallID  string      `json:"call_id"`
	Tool    string      `json:"tool"`
	Args    string      `json:"args"`
	Preview []tool.Line `json:"preview"`
	// Grant is the destination this call could be allowed under for the rest
	// of the turn, when it can be. The page offers one checkbox per distinct
	// value; the server and the loop each refuse any other.
	Grant string `json:"grant,omitempty"`
}

// batchApprover is the Agent.ApproveBatch closure for one streaming turn: the
// gated calls one model step makes to one tool go out as a single card.
//
// Every path that is not an explicit yes denies, which is the rule the terminal
// approver already follows and the reason it is written as a switch on the
// reasons rather than a happy path with error handling: a closed tab, a person
// who never answers, and a server that restarted are all "no", and none of them
// should be distinguishable from "no" by the tool that was asked for.
func (s *Server) batchApprover(runID string, out *sseWriter, workspace string, isBrief bool) func(context.Context, []llm.ToolCall, []string) (loop.BatchDecision, error) {
	return func(ctx context.Context, calls []llm.ToolCall, keys []string) (loop.BatchDecision, error) {
		denied := loop.BatchDecision{Approved: make([]bool, len(calls))}
		if len(calls) == 0 {
			return denied, nil
		}
		// The card is named by its first call: call IDs are provider-assigned
		// and unique within a turn, so the first one identifies the batch.
		id := calls[0].ID
		ids := make([]string, len(calls))
		card := make([]cardCall, len(calls))
		for i, c := range calls {
			ids[i] = c.ID
			card[i] = cardCall{
				CallID: c.ID, Tool: c.Name, Args: compactJSON(c.Args),
				// What the call would do, in words: the path and the lines an
				// edit changes, the URL in full, a note as a sentence. The args
				// stay because the page still shows them under the preview —
				// this replaces reading them, not the ability to.
				Preview: tool.Preview(c.Name, c.Args, workspace),
			}
			if i < len(keys) {
				card[i].Grant = keys[i]
			}
		}

		key := approvalKey(runID, id)
		ch := s.approvals.wait(key, ids, keys)
		defer s.approvals.forget(key)

		out.event("approval_required", map[string]any{
			"run_id":  runID,
			"call_id": id,
			"tool":    calls[0].Name,
			"calls":   card,
		})

		timer := time.NewTimer(s.approvalTimeout)
		defer timer.Stop()

		select {
		case ans := <-ch:
			if !ans.approve {
				return denied, nil
			}
			skip := map[string]bool{}
			for _, d := range ans.deny {
				skip[d] = true
			}
			d := loop.BatchDecision{Approved: make([]bool, len(calls)), Grants: ans.grant}
			for i, c := range calls {
				d.Approved[i] = !skip[c.ID]
			}
			return d, nil
		case <-ctx.Done():
			// The tab closed or the request was cancelled. Nobody is reading the
			// prompt that was just sent, so nobody is going to answer it.
			return denied, ctx.Err()
		case <-timer.C:
			if isBrief {
				out.event("approval_waiting", map[string]any{"call_id": id, "tool": calls[0].Name})
				return denied, errApprovalWaiting
			}
			out.event("approval_timeout", map[string]any{"call_id": id, "tool": calls[0].Name})
			return denied, nil
		}
	}
}

// approver is batchApprover for one call: a card with a single line and
// nothing to grant. Kept as the shape Agent.Approve expects, so anything that
// asks about one call at a time gets the same card and the same answers.
func (s *Server) approver(runID string, out *sseWriter, workspace string, isBrief bool) func(context.Context, llm.ToolCall) (bool, error) {
	batch := s.batchApprover(runID, out, workspace, isBrief)
	return func(ctx context.Context, c llm.ToolCall) (bool, error) {
		d, err := batch(ctx, []llm.ToolCall{c}, []string{""})
		return len(d.Approved) == 1 && d.Approved[0], err
	}
}

type approveRequest struct {
	RunID   string   `json:"run_id"`
	CallID  string   `json:"call_id"`
	Approve bool     `json:"approve"`
	Deny    []string `json:"deny,omitempty"`
	Grant   []string `json:"grant,omitempty"`
}

// handleApprove records a decision for a card some turn is blocked on.
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
	// A refusal allows nothing, so a Deny that also carries grants is not an
	// answer anyone meant; drop them rather than guess.
	if !req.Approve {
		req.Grant = nil
	}

	found, err := s.approvals.decideBatch(approvalKey(req.RunID, req.CallID),
		answer{approve: req.Approve, deny: req.Deny, grant: req.Grant})
	if !found {
		// Gone rather than not-found: the question existed and no longer does,
		// which is what a timeout or a closed tab leaves behind. A client that
		// answered too late should be told that, not told it asked wrongly.
		httpError(w, http.StatusGone, "nothing is waiting on that call; it timed out or was already answered")
		return
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "approved": req.Approve})
}
