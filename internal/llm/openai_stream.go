package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

var _ Streamer = (*OpenAI)(nil)

// sseDone is the sentinel the protocol ends with. It is not JSON, and decoding
// it is the classic way a streaming client dies on the last line.
const sseDone = "[DONE]"

// ErrStreamCut is a stream that ended without the provider saying the answer
// was over: no [DONE] and no finish_reason. The connection closed early, and
// what arrived is part of an answer, not an answer.
var ErrStreamCut = errors.New("openai: the connection closed before the answer was finished")

// maxSSELine bounds one event. bufio.Scanner's default is 64KB, which a large
// tool-call argument can exceed — and the failure is a truncated line that
// fails to parse, not an obvious error.
const maxSSELine = 1 << 20

// Stream sends the request with stream=true and yields chunks as they arrive.
//
// Retried automatically before the first byte: transient server errors (429,
// 500, 502, 503, 504, 529) and network failures are retried via send before
// response headers are accepted. Once 200 OK headers arrive and streaming
// begins, mid-stream disconnects are not retried (doing so would replay printed
// tokens); instead, ErrStreamCut is yielded to the caller for recovery.
func (o *OpenAI) Stream(ctx context.Context, req Request) (iter.Seq2[Chunk, error], error) {
	wire, err := toWire(req)
	if err != nil {
		return nil, err
	}
	wire.Stream = true
	wire.StreamOptions = &oaStreamOptions{IncludeUsage: true}

	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}

	resp, err := o.send(ctx, payload, true)
	if err != nil {
		return nil, err
	}

	return func(yield func(Chunk, error) bool) {
		// Runs however the loop ends — exhausted, an early break, or a yield
		// returning false. That is the reason for range-over-func here rather
		// than a channel: cleanup cannot be forgotten by the consumer.
		defer func() {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
		}()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)

		// Whether the provider said the answer was over. A connection closed
		// cleanly mid-answer reads as an ordinary end of body, and without
		// this the half-sentence it carried was accepted as the whole answer
		// and saved — found by a fake that closed after "IHSG closed at 7,1".
		finished := false

		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue // comments, keep-alives, and the blank line between events
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == sseDone {
				return
			}

			var raw oaStreamChunk
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				yield(Chunk{}, fmt.Errorf("openai: decode stream chunk: %w", err))
				return
			}
			if raw.Error != nil {
				if raw.Error.contextLength() {
					yield(Chunk{}, fmt.Errorf("openai: stream error: %s: %w", raw.Error.Message, ErrContextLength))
					return
				}
				yield(Chunk{}, fmt.Errorf("openai: stream error: %s", raw.Error.Message))
				return
			}

			for _, c := range chunksOf(raw) {
				if c.Stop != "" {
					finished = true
				}
				if !yield(c, nil) {
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			if ctx.Err() != nil {
				yield(Chunk{}, ctx.Err())
				return
			}
			yield(Chunk{}, fmt.Errorf("openai: read stream: %w", err))
			return
		}
		// [DONE] returns above. A finish_reason without [DONE] is accepted,
		// because the answer is whole and some servers omit the sentinel;
		// neither is a cut connection.
		if !finished {
			yield(Chunk{}, ErrStreamCut)
		}
	}, nil
}

// chunksOf flattens one wire chunk into zero or more Chunks.
//
// Zero is normal and has to be tolerated: the first event of a stream usually
// carries only `{"role":"assistant"}`, and keep-alives carry nothing at all.
// More than one happens when a single event holds several tool-call fragments.
func chunksOf(raw oaStreamChunk) []Chunk {
	var out []Chunk

	// Who answered, emitted as its own chunk so the accumulator can keep it
	// without every other chunk having to carry it. Usually only the first
	// chunk of a stream says.
	if raw.Model != "" || raw.Provider != "" {
		out = append(out, Chunk{Model: raw.Model, Provider: raw.Provider})
	}

	for _, ch := range raw.Choices {
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			out = append(out, Chunk{Text: *ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			d := ToolDelta{
				Index: tc.Index,
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Args:  tc.Function.Arguments,
			}
			out = append(out, Chunk{ToolCall: &d})
		}
		if ch.FinishReason != "" {
			// hasToolCalls is false here on purpose: this event says only how
			// the model stopped, and whether the turn was a tool-use turn is
			// something the accumulator knows once every fragment has landed.
			out = append(out, Chunk{Stop: stopReason(ch.FinishReason, false)})
		}
	}

	if raw.Usage != nil {
		out = append(out, Chunk{Usage: raw.Usage.usage()})
	}
	return out
}
