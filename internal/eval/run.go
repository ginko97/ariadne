package eval

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ginko97/ariadne/internal/loop"
)

// TaskEnv is what a task decides about the agent that runs it.
type TaskEnv struct {
	RunID    string
	MaxSteps int
	// Workspace is the task's own folder when it works on files, and "" when
	// it does not; the factory then uses whatever it would have.
	Workspace string
	// Approve names the tools whose approval cards are answered yes; every
	// other card is answered no. Never nil-means-all.
	Approve []string
}

// AgentFactory builds an agent for one task.
//
// The indirection keeps this package free of providers, tools and keys: eval
// knows how to score a run, not how to construct one. cmd/ariadne supplies the
// wiring, and a test supplies a Fake.
//
// The run id is passed in rather than read from somewhere afterwards, because
// anything the factory opens per run — a trace file, say — has to be named for
// the run it belongs to. An earlier version let the factory read a shared
// variable set by newRunID, and since the runner builds the agent before the
// state, every trace was written into the previous task's directory.
type AgentFactory func(model string, env TaskEnv) *loop.Agent

// NewRunID names one task's run. Injected so tests are deterministic and so the
// caller controls the id scheme that becomes a directory under runs/.
type NewRunID func(model, taskID string) string

// RunTasks runs every task against one model and scores each, repeat times.
//
// Every task runs even when earlier ones fail — a scorecard with a hole in it
// is not comparable to one without, and a single provider hiccup should not
// discard the other twenty-nine results.
//
// A task run more than once passes only if every attempt passed. Models are
// not consistent — this project has seen one score 4/6 and then 3/6 on
// identical code twenty minutes apart — so one pass on one attempt says less
// than it looks, and "passed 2/3" is a finding in itself.
func RunTasks(ctx context.Context, tasks []Task, model string, newAgent AgentFactory, newRunID NewRunID, repeat int) []Result {
	if newAgent == nil || newRunID == nil {
		return nil
	}
	if repeat < 1 {
		repeat = 1
	}
	results := make([]Result, 0, len(tasks))

	for _, t := range tasks {
		// Cancellation stops the sweep, but keeps what has been scored so far.
		if ctx.Err() != nil {
			break
		}
		var agg Result
		firstFail := ""
		for a := 0; a < repeat && ctx.Err() == nil; a++ {
			r := runOnce(ctx, t, model, newAgent, newRunID)
			agg.Attempts++
			agg.Cost += r.Cost
			agg.Steps = max(agg.Steps, r.Steps)
			if r.Pass {
				agg.Passes++
				if firstFail == "" {
					agg.Answer, agg.RunID, agg.Provider = r.Answer, r.RunID, r.Provider
				}
				continue
			}
			if firstFail == "" {
				// The failing attempt is the one worth reading.
				firstFail = r.Reason
				agg.Answer, agg.RunID, agg.Provider = r.Answer, r.RunID, r.Provider
			}
		}
		agg.TaskID = t.ID
		agg.Pass = agg.Attempts > 0 && agg.Passes == agg.Attempts
		agg.Reason = firstFail
		if agg.Attempts > 1 && !agg.Pass {
			agg.Reason = fmt.Sprintf("%s (passed %d/%d)", firstFail, agg.Passes, agg.Attempts)
		}
		if repeat == 1 {
			agg.Attempts, agg.Passes = 0, 0
		}
		results = append(results, agg)
	}

	return results
}

// runOnce runs one attempt of t in a folder of its own, if it needs one.
func runOnce(ctx context.Context, t Task, model string, newAgent AgentFactory, newRunID NewRunID) Result {
	maxSteps := t.MaxSteps
	if maxSteps == 0 {
		maxSteps = 10
	}
	env := TaskEnv{RunID: newRunID(model, t.ID), MaxSteps: maxSteps, Approve: append([]string{}, t.Approve...)}
	state := loop.NewState(env.RunID, t.Prompt)

	var fixtures map[string][]byte
	if t.usesFiles() {
		ws, fx, err := prepareWorkspace(t)
		if err != nil {
			return Score(t, state, "", fmt.Errorf("eval: fixtures: %w", err))
		}
		defer os.RemoveAll(ws)
		env.Workspace, fixtures = ws, fx
	}

	agent := newAgent(model, env)
	if agent == nil {
		return Score(t, state, "", errors.New("eval: agent factory returned nil"))
	}
	answer, err := agent.Run(ctx, state)
	r := Score(t, state, answer, err)
	if r.Pass && env.Workspace != "" {
		if reason := checkFiles(t, env.Workspace, fixtures); reason != "" {
			r.Pass, r.Reason = false, reason
		}
	}
	return r
}
