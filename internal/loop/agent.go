package loop

import (
	"context"
	"errors"
	"fmt"

	"github.com/ginko97/ariadne/internal/llm"
)

// Sentinel errors, so tests and callers use errors.Is instead of matching strings.
var (
	ErrStepLimit    = errors.New("loop: step limit exceeded")
	ErrCostLimit    = errors.New("loop: cost limit exceeded")
	ErrTruncated    = errors.New("loop: model output truncated")
	ErrNoToolRunner = errors.New("loop: model requested a tool but no ToolRunner is configured")
	ErrEmptyToolUse = errors.New("loop: stop=tool_use but response carried no tool_use blocks")
)

// Price is USD per million tokens, per model.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost converts provider-reported tokens into money. Never a local tokenizer —
// the provider's own count is the only number that matches the bill.
func (p Price) Cost(u llm.Usage) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerMTok +
		float64(u.OutputTokens)/1e6*p.OutputPerMTok
}

// ToolRunner executes one tool call. tool.Registry.Call satisfies it, and so
// does a closure in a test — the loop does not care which, and deliberately
// does not import internal/tool. That is why ToolResult lives in llm.
type ToolRunner func(ctx context.Context, call llm.ToolCall) (llm.ToolResult, error)

type Agent struct {
	Provider llm.Provider
	Model    string
	Tools    []llm.ToolDef
	RunTool  ToolRunner

	MaxSteps int // 0 = unlimited (tests only; never in production)
	MaxCost  float64
	Price    Price
}

// Run drives the agent loop until the model stops, a limit trips, or ctx is cancelled.
//
// It mutates s as it goes, so a caller holding s can checkpoint it after any step
// (week 5) and can inspect Steps and Cost after an error.
func (a *Agent) Run(ctx context.Context, s *State) (string, error) {
	for {
		// --- guards: top of every iteration, before any work ---
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if a.MaxSteps > 0 && s.Steps >= a.MaxSteps {
			return "", fmt.Errorf("%w: %d steps", ErrStepLimit, s.Steps)
		}
		if a.MaxCost > 0 && s.Cost >= a.MaxCost {
			return "", fmt.Errorf("%w: $%.4f spent", ErrCostLimit, s.Cost)
		}

		resp, err := a.Provider.Complete(ctx, llm.Request{
			Model:    a.Model,
			Messages: s.Messages,
			Tools:    a.Tools,
		})
		if err != nil {
			return "", err
		}

		// Charged whether or not the turn was useful.
		s.Steps++
		s.Cost += a.Price.Cost(resp.Usage)

		// Whatever the model said is now part of the conversation, in every branch.
		s.Messages = append(s.Messages, llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: resp.Blocks,
		})

		switch resp.Stop {
		case llm.StopEnd:
			return resp.Text(), nil
		case llm.StopMaxToken:
			return "", ErrTruncated
		case llm.StopToolUse:
			// falls through to tool execution below
		default:
			return "", fmt.Errorf("loop: unknown stop reason %q", resp.Stop)
		}

		calls := resp.ToolCalls()
		if len(calls) == 0 {
			return "", ErrEmptyToolUse
		}
		if a.RunTool == nil {
			return "", ErrNoToolRunner
		}

		// Serial on purpose. Parallel tools land in week 10, after resume works —
		// a half-finished parallel batch is a different checkpoint problem.
		results := make([]llm.Block, 0, len(calls))
		for _, c := range calls {
			// Two ways a tool reports failure, and they mean different things:
			// res.IsError is "it ran and failed" (the model can react); a non-nil
			// err is "it could not be reached at all".
			res, err := a.RunTool(ctx, c)
			b := llm.Block{
				Type:    llm.BlockToolResult,
				CallID:  c.ID,
				Content: res.Content,
				IsError: res.IsError,
			}
			if err != nil {
				// Still information for the model, not a dead run — it can retry,
				// pick another tool, or give up. Cancellation is the exception:
				// that is the caller leaving.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", ctxErr
				}
				b.Content = err.Error()
				b.IsError = true
			}
			results = append(results, b)
		}

		// All results for this step go back as ONE user message, so the count of
		// messages stays predictable per step — which matters when resuming.
		s.Messages = append(s.Messages, llm.Message{
			Role:   llm.RoleUser,
			Blocks: results,
		})
	}
}
