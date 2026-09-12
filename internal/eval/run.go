package eval

import (
	"context"

	"github.com/ginko97/ariadne/internal/loop"
)

// AgentFactory builds an agent for one task.
//
// The indirection keeps this package free of providers, tools and keys: eval
// knows how to score a run, not how to construct one. cmd/ariadne supplies the
// wiring, and a test supplies a Fake.
type AgentFactory func(model string, maxSteps int) *loop.Agent

// NewRunID names one task's run. Injected so tests are deterministic and so the
// caller controls the id scheme that becomes a directory under runs/.
type NewRunID func(model, taskID string) string

// RunTasks runs every task against one model and scores each.
//
// Every task runs even when earlier ones fail — a scorecard with a hole in it
// is not comparable to one without, and a single provider hiccup should not
// discard the other twenty-nine results.
func RunTasks(ctx context.Context, tasks []Task, model string, newAgent AgentFactory, newRunID NewRunID) []Result {
	results := make([]Result, 0, len(tasks))

	for _, t := range tasks {
		// Cancellation stops the sweep, but keeps what has been scored so far.
		if ctx.Err() != nil {
			break
		}

		maxSteps := t.MaxSteps
		if maxSteps == 0 {
			maxSteps = 10
		}

		agent := newAgent(model, maxSteps)
		state := loop.NewState(newRunID(model, t.ID), t.Prompt)

		answer, err := agent.Run(ctx, state)
		results = append(results, Score(t, state, answer, err))
	}

	return results
}
