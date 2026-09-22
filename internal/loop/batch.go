package loop

import (
	"context"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// BatchDecision is the operator's answer to one batch of gated calls.
type BatchDecision struct {
	// Approved has one entry per call, in the order the calls were asked.
	Approved []bool
	// Grants are keys the operator allowed for the rest of this turn. Only a
	// key the loop offered for a call in this batch is honoured; anything else
	// is ignored, so a front end cannot widen a grant beyond what it was shown.
	Grants []string
}

// approvalBatch is the gated calls to one tool from one model step, with the
// grant key the policy produced for each ("" where none may be granted).
type approvalBatch struct {
	calls []llm.ToolCall
	keys  []string
}

// grantKey is the key a turn-scoped grant for c would be filed under, or
// ("", false) if c may not be granted at all. The policy is the agent's
// GrantKey: the loop does not know which tools reach the network, and must not
// guess.
func (a *Agent) grantKey(c llm.ToolCall) (string, bool) {
	if a.GrantKey == nil {
		return "", false
	}
	return a.GrantKey(c)
}

// granted reports whether c is covered by a grant made earlier in this turn,
// and by which key.
func (a *Agent) granted(c llm.ToolCall) (string, bool) {
	key, ok := a.grantKey(c)
	if !ok || key == "" {
		return "", false
	}
	return key, a.grants[key]
}

// decide asks the operator about the gated calls of one step and returns a
// decision per call, keyed by call ID.
//
// Calls are asked in batches, one per tool in the order the tools first appear,
// because that is how the model asked for them and a card listing six fetches
// is read once where six cards are read less each time. Without ApproveBatch
// every call is asked on its own, which is exactly the behaviour before
// batching existed — the terminal and the eval harness keep it.
func (a *Agent) decide(ctx context.Context, s *State, gated []llm.ToolCall) (map[string]bool, error) {
	out := make(map[string]bool, len(gated))
	if len(gated) == 0 {
		return out, nil
	}

	if a.ApproveBatch == nil {
		for _, c := range gated {
			ok, err := a.askApproval(ctx, c)
			if err != nil {
				return nil, err
			}
			out[c.ID] = ok
		}
		return out, nil
	}

	var order []string
	batches := map[string]*approvalBatch{}
	for _, c := range gated {
		b, ok := batches[c.Name]
		if !ok {
			b = &approvalBatch{}
			batches[c.Name] = b
			order = append(order, c.Name)
		}
		key, _ := a.grantKey(c)
		b.calls = append(b.calls, c)
		b.keys = append(b.keys, key)
	}

	for _, name := range order {
		b := batches[name]
		d, err := a.ApproveBatch(ctx, b.calls, b.keys)
		if err != nil {
			return nil, err
		}
		// A decision of the wrong length is a front end that did not answer
		// the question it was asked. Every call it did not answer is a no.
		for i, c := range b.calls {
			out[c.ID] = i < len(d.Approved) && d.Approved[i]
		}
		a.recordGrants(s, b, d)
	}
	return out, nil
}

// recordGrants files the grants the operator made, honouring only keys that
// were offered for a call in this batch — and only for calls the operator
// actually approved, since granting a destination while refusing the request
// to it is not a decision anyone makes on purpose.
func (a *Agent) recordGrants(s *State, b *approvalBatch, d BatchDecision) {
	if len(d.Grants) == 0 {
		return
	}
	offered := map[string]bool{}
	for i, k := range b.keys {
		if k != "" && i < len(d.Approved) && d.Approved[i] {
			offered[k] = true
		}
	}
	tool := ""
	if len(b.calls) > 0 {
		tool = b.calls[0].Name
	}
	for _, g := range d.Grants {
		if !offered[g] {
			// Refused, and said so in the trace: a key nobody was shown, or one
			// whose only request was denied, is not a decision anybody made.
			a.emit(trace.Event{
				Kind: trace.KindHostGrant, Step: s.Steps, Tool: tool,
				Content: "refused: " + g + " was not offered for an approved call in this batch", IsError: true,
			})
			continue
		}
		if a.grants == nil {
			a.grants = map[string]bool{}
		}
		if a.grants[g] {
			continue
		}
		a.grants[g] = true
		a.emit(trace.Event{
			Kind: trace.KindHostGrant, Step: s.Steps, Tool: tool,
			Content: g + " allowed until this turn ends",
		})
	}
}
