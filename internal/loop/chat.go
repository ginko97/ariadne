package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// Multi-turn: a conversation is a run you keep adding to.
//
// Everything needed for a durable chat already existed — checkpointing per tool
// call, resume, compaction, cost accounting, memory, traces — and exactly one
// verb was missing: put a new message from the person into a run that has
// already stopped. Run continues from whatever State holds, so a second turn is
// an append followed by another Run, and a conversation is not a new kind of
// object. It is the same job, still unfinished.
//
// That is why `ariadne traces` lists conversations without being taught to: they
// were never separate from runs.

// ErrTurnInFlight is returned when a message is added to a run that still has
// tool calls waiting for results.
var ErrTurnInFlight = errors.New("loop: cannot add a message while tool calls are pending")

// AddUserMessage appends a turn from the person.
//
// Refused while a batch is unfinished, and that refusal is the whole reason this
// is a method rather than an append at the call site. A user turn inserted
// between an assistant's tool_use and its results is two separate failures: the
// provider rejects the request outright, and pendingToolCalls — which reads the
// completion record out of the conversation itself — would no longer find the
// pair it needs, so a resumed run could re-fire a tool that already ran.
//
// The caller finishes the batch first by calling Run, which is what it would do
// anyway.
// SetModel changes the model for subsequent turns. Refused while a tool batch
// is unfinished to protect crash recovery replay fidelity.
func (s *State) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("loop: model cannot be empty")
	}
	if pending := s.pendingToolCalls(); len(pending) > 0 {
		return fmt.Errorf("%w: cannot switch model while tool calls are pending", ErrTurnInFlight)
	}
	s.Model = model
	return nil
}

func (s *State) AddUserMessage(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("loop: a message cannot be empty")
	}
	if pending := s.pendingToolCalls(); len(pending) > 0 {
		return fmt.Errorf("%w: %d unfinished", ErrTurnInFlight, len(pending))
	}

	s.Task = text
	s.Messages = append(s.Messages, llm.Message{
		Role:   llm.RoleUser,
		Blocks: []llm.Block{{Type: llm.BlockText, Text: text}},
	})
	return nil
}

// ChatTurn adds a message and runs until the model stops.
//
// One turn of a conversation, and the unit a step ceiling applies to — MaxSteps
// bounds this call, not the conversation, or a long chat would die of its own
// history. Cost stays cumulative, because a conversation that has spent too much
// has spent too much however many turns it took; MaxCost is what bounds the
// whole thing.
//
// Checkpointing is unchanged and per tool call, so closing the terminal
// mid-answer loses nothing and `resume` picks the turn up where it stopped.
func (a *Agent) ChatTurn(ctx context.Context, s *State, text string) (string, error) {
	if a.Model != "" && s.Model != "" && a.Model != s.Model {
		if err := s.SetModel(a.Model); err != nil {
			return "", err
		}
	}
	if err := s.AddUserMessage(text); err != nil {
		return "", err
	}
	return a.Run(ctx, s)
}

// Turns counts the messages from the person, which is what a conversation
// looks like from outside — the assistant turns and tool results in between are
// how it was answered, not how long it is.
func (s *State) Turns() int {
	n := 0
	for _, m := range s.Messages {
		if m.Role != llm.RoleUser || isToolResults(m) {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
				n++
				break
			}
		}
	}
	return n
}
