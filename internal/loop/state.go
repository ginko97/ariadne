package loop

import "github.com/ginko97/ariadne/internal/llm"

// State is everything about one run.
//
// In week 5 this struct is what gets written to disk after every step, and what
// `ariadne resume` loads back. That is why it holds no interfaces, no channels,
// and no funcs — every field must survive a JSON round-trip unchanged.
//
// The schema version lives on the checkpoint envelope, not here: it describes
// the file format, not the run.
type State struct {
	RunID    string        `json:"run_id"`
	Task     string        `json:"task"`
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
