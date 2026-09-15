package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// runsResponse is the conversation list.
//
// Skipped is reported rather than dropped. A checkpoint that will not parse is
// one conversation the person cannot reach, and a list that is quietly shorter
// than the truth is the failure mode this project has already paid for twice —
// the corrupt trace line that hid every event behind it, and the scorer that
// passed by matching a substring. A number they can see is the difference
// between a missing conversation and a conversation that was never there.
type runsResponse struct {
	Runs    []runRow `json:"runs"`
	Skipped int      `json:"skipped"`
}

// runRow is deliberately not loop.Summary. The API shape should not change
// because a field was added to the checkpoint, and the checkpoint should not
// gain a json tag because a browser wanted one.
type runRow struct {
	RunID    string  `json:"run_id"`
	Title    string  `json:"title"`
	Model    string  `json:"model"`
	Turns    int     `json:"turns"`
	Steps    int     `json:"steps"`
	Messages int     `json:"messages"`
	Cost     float64 `json:"cost_usd"`
	Updated  string  `json:"updated"` // RFC3339
}

// handleRuns lists conversations, most recently active first.
//
// This is what makes the release's exit test reachable from a browser: talk to
// it yesterday, come back today, and find the conversation again. Resuming
// already worked over HTTP, but only for somebody who had kept the run id —
// which in practice meant reading it out of a done event by hand.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	rows, skipped, err := s.Store.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}

	// limit is a courtesy for a long history, not a pager. When the list
	// outgrows one response the answer is a cursor, and inventing half of one
	// now would be a shape to keep working around later.
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		if n < len(rows) {
			rows = rows[:n]
		}
	}

	out := runsResponse{Runs: make([]runRow, 0, len(rows)), Skipped: skipped}
	for _, r := range rows {
		out.Runs = append(out.Runs, runRow{
			RunID:    r.RunID,
			Title:    r.Task,
			Model:    r.Model,
			Turns:    r.Turns,
			Steps:    r.Steps,
			Messages: r.Messages,
			Cost:     r.Cost,
			Updated:  r.Updated.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		// Headers are already out; nothing useful left to say to this client.
		return
	}
}
