package llm

import (
	"context"
	"encoding/json"
	"iter"
	"sort"
)

// Streaming, and why the loop does not know about it.
//
// A stream is a different shape from Complete: many pieces instead of one
// answer. The obvious move is to give the loop a second path, and it is the
// wrong one — cost accounting, the stop-reason switch, checkpointing and
// compaction all take a whole Response, and a second path means two of each
// that can disagree.
//
// So streaming lives entirely at the provider boundary. Streaming (below)
// consumes the stream, hands every delta to a callback as it arrives, and
// returns exactly the Response that Complete would have returned. The loop is
// unchanged and cannot tell the difference, which is the point: tokens on a
// terminal are a display concern, and none of the run's semantics should turn
// on whether somebody is watching.

// Chunk is one incremental piece of a streamed response.
//
// Fields are additive rather than a union: a single chunk can carry a text
// delta and a tool-call delta at once, and the last one carries the stop reason
// and usage with no content at all.
type Chunk struct {
	Text     string
	ToolCall *ToolDelta
	Stop     StopReason
	Usage    Usage
}

// ToolDelta is a fragment of one tool call.
//
// Index, not ID, is the identity. The provider sends the id and name once, on
// the first fragment, and every later fragment for the same call carries only
// the index and a slice of the arguments — so anything keyed on id loses every
// fragment after the first.
type ToolDelta struct {
	Index int
	ID    string
	Name  string
	// Args is a *fragment* of the JSON arguments, not valid JSON on its own.
	// A call to calc arrives as roughly `{"expr"`, `:"2+2"`, `}` across three
	// chunks, so nothing can be parsed until the stream ends. This is the part
	// of streaming that actually has teeth.
	Args string
}

// Streamer is a Provider that can also deliver a response incrementally.
//
// Deliberately a separate interface rather than a second method on Provider:
// Fake has no stream and should not have to grow a stub, and a provider that
// cannot stream should fail to satisfy this rather than satisfy it badly. The
// caller type-asserts, the way http.Flusher works.
type Streamer interface {
	Stream(ctx context.Context, req Request) (iter.Seq2[Chunk, error], error)
}

// accumulator assembles chunks back into the Response the loop expects.
//
// Kept apart from the SSE parsing so the hard half — fragment reassembly — is
// testable without a transport.
type accumulator struct {
	text  string
	calls map[int]*ToolDelta
	stop  StopReason
	usage Usage
}

func newAccumulator() *accumulator {
	return &accumulator{calls: map[int]*ToolDelta{}}
}

func (a *accumulator) add(c Chunk) {
	a.text += c.Text

	if d := c.ToolCall; d != nil {
		call, ok := a.calls[d.Index]
		if !ok {
			call = &ToolDelta{Index: d.Index}
			a.calls[d.Index] = call
		}
		// id and name arrive once and must not be clobbered by later fragments
		// that carry only arguments.
		if d.ID != "" {
			call.ID = d.ID
		}
		if d.Name != "" {
			call.Name = d.Name
		}
		call.Args += d.Args
	}

	if c.Stop != "" {
		a.stop = c.Stop
	}
	// Usage arrives in its own final chunk and is the only place a streamed
	// run learns what it spent. Requesting it is not the default — see
	// stream_options in openai.go.
	if c.Usage.InputTokens > 0 || c.Usage.OutputTokens > 0 || c.Usage.Cost > 0 {
		a.usage = c.Usage
	}
}

func (a *accumulator) response() Response {
	var blocks []Block
	if a.text != "" {
		blocks = append(blocks, Block{Type: BlockText, Text: a.text})
	}

	// Map iteration is unordered and tool calls are positional, so sort by the
	// index the provider assigned. Without this the same stream can produce
	// calls in a different order on every run, and a resumed batch would not
	// match the one it is finishing.
	idx := make([]int, 0, len(a.calls))
	for i := range a.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)

	for _, i := range idx {
		d := a.calls[i]
		args := d.Args
		if args == "" {
			args = "{}" // an empty string is not valid JSON; servers reject it
		}
		blocks = append(blocks, Block{
			Type: BlockToolUse, ID: d.ID, Name: d.Name, Args: json.RawMessage(args),
		})
	}

	stop := a.stop
	if stop == "" {
		stop = StopEnd
	}
	// A stream that produced tool calls is a tool-use turn whatever the
	// finish_reason said. Gateways in front of a model report "stop" here often
	// enough that the non-streaming path already has this rule; the streaming
	// path needs it for the same reason.
	if stop == StopEnd {
		for _, b := range blocks {
			if b.Type == BlockToolUse {
				stop = StopToolUse
				break
			}
		}
	}

	return Response{Blocks: blocks, Stop: stop, Usage: a.usage}
}

// Streaming turns a Streamer into a Provider.
//
// The loop gets one Response, exactly as if it had called Complete; OnDelta
// sees each piece as it arrives. Everything that makes a run a run — cost,
// checkpoints, compaction, the stop switch — keeps operating on whole
// responses, and nothing about a run changes because somebody is watching it.
type Streaming struct {
	S Streamer
	// OnDelta is called for every chunk, in order, before Complete returns.
	// Synchronous on purpose: a token printed after the answer is worse than no
	// token at all.
	OnDelta func(Chunk)
}

var _ Provider = Streaming{}

func (p Streaming) Complete(ctx context.Context, req Request) (Response, error) {
	seq, err := p.S.Stream(ctx, req)
	if err != nil {
		return Response{}, err
	}

	acc := newAccumulator()
	for chunk, err := range seq {
		if err != nil {
			return Response{}, err
		}
		if p.OnDelta != nil {
			p.OnDelta(chunk)
		}
		acc.add(chunk)
	}
	// The context is checked after the range rather than inside it: breaking
	// out early would skip the iterator's own cleanup, and the HTTP body has to
	// be drained and closed for the connection to go back to the pool.
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	return acc.response(), nil
}
