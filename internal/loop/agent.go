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

	// Checkpoint, if set, is called after every step and once more before Run
	// returns. A failure fails the run: a run that cannot be checkpointed
	// cannot be resumed, and continuing would be lying about durability.
	Checkpoint func(*State) error

	MaxSteps int // 0 = unlimited (tests only; never in production)
	MaxCost  float64
	Price    Price
}

// Run drives the agent loop until the model stops, a limit trips, or ctx is cancelled.
//
// It mutates s as it goes, so a caller holding s can checkpoint it after any step
// (week 5) and can inspect Steps and Cost after an error.
func (a *Agent) Run(ctx context.Context, s *State) (string, error) {
	// Record the model once. A resumed state already carries it, and the caller
	// is expected to have built the provider from it.
	if s.Model == "" {
		s.Model = a.Model
	}

	for {
		// --- guards: top of every iteration, before any work ---
		if err := ctx.Err(); err != nil {
			return "", err
		}

		// Finish a half-executed batch before asking the model anything. On a
		// fresh run this is always empty; on resume it is the whole point —
		// re-asking would mint new call IDs and re-fire tools that already ran.
		if pending := s.pendingToolCalls(); len(pending) > 0 {
			if err := a.runCalls(ctx, s, pending); err != nil {
				return "", err
			}
			continue
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
			// The assistant turn is already appended, so this write captures the
			// finished conversation. Without it, resume would replay the last step.
			if err := a.checkpoint(s); err != nil {
				return "", err
			}
			return resp.Text(), nil
		case llm.StopMaxToken:
			return "", ErrTruncated
		case llm.StopToolUse:
			// falls through to tool execution below
		default:
			return "", fmt.Errorf("loop: unknown stop reason %q", resp.Stop)
		}

		if len(resp.ToolCalls()) == 0 {
			return "", ErrEmptyToolUse
		}
		if a.RunTool == nil {
			return "", ErrNoToolRunner
		}

		// Requested but not executed. A crash between here and the first result
		// is recoverable: the calls are on disk with their original IDs, and the
		// top of the next iteration picks them up. Then loop — there is exactly
		// one tool-execution path, shared by fresh and resumed batches.
		if err := a.checkpoint(s); err != nil {
			return "", err
		}
	}
}

// runCalls executes a batch, appending each result and checkpointing as it goes.
//
// Serial on purpose. Parallel tools land in week 10; a concurrent batch needs a
// different completion record than "append in order".
func (a *Agent) runCalls(ctx context.Context, s *State, calls []llm.ToolCall) error {
	if a.RunTool == nil {
		return ErrNoToolRunner
	}

	// Open the results message unless resume left one half-filled.
	if last := s.Messages[len(s.Messages)-1]; !isToolResults(last) {
		s.Messages = append(s.Messages, llm.Message{Role: llm.RoleUser})
	}
	i := len(s.Messages) - 1

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
				return ctxErr
			}
			b.Content = err.Error()
			b.IsError = true
		}
		s.Messages[i].Blocks = append(s.Messages[i].Blocks, b)

		// Per call, not per step. This is the write that stops a resumed run
		// re-firing a tool whose side effect already happened.
		if err := a.checkpoint(s); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) checkpoint(s *State) error {
	if a.Checkpoint == nil {
		return nil
	}
	if err := a.Checkpoint(s); err != nil {
		return fmt.Errorf("loop: checkpoint failed at step %d: %w", s.Steps, err)
	}
	return nil
}
