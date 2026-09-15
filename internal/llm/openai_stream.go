package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

var _ Streamer = (*OpenAI)(nil)

// sseDone is the sentinel the protocol ends with. It is not JSON, and decoding
// it is the classic way a streaming client dies on the last line.
const sseDone = "[DONE]"

// maxSSELine bounds one event. bufio.Scanner's default is 64KB, which a large
// tool-call argument can exceed — and the failure is a truncated line that
// fails to parse, not an obvious error.
const maxSSELine = 1 << 20

// Stream sends the request with stream=true and yields chunks as they arrive.
//
// Deliberately not retried, unlike Complete. A 429 before the first byte could
// be retried safely, but once tokens have been handed to OnDelta they have
// been printed, and replaying the request would print a second answer over the
// top of the first. Streaming and automatic retry want opposite things from a
// half-finished response, so this one leaves the decision to the caller.
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

	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	for k, vs := range o.Extra {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("openai: request failed: %w", err)
	}

	// A failure here is an ordinary body, not a stream, and the caller wants it
	// before iterating rather than as the first yielded error.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyLength*2))
		resp.Body.Close()
		return nil, fmt.Errorf("openai: %s: %s", resp.Status, truncate(body, maxErrBodyLength))
	}

	return func(yield func(Chunk, error) bool) {
		// Runs however the loop ends — exhausted, an early break, or a yield
		// returning false. That is the reason for range-over-func here rather
		// than a channel: cleanup cannot be forgotten by the consumer.
		defer func() {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)

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
				yield(Chunk{}, fmt.Errorf("openai: stream error: %s", raw.Error.Message))
				return
			}

			for _, c := range chunksOf(raw) {
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
		out = append(out, Chunk{Usage: Usage{
			InputTokens:  raw.Usage.PromptTokens,
			OutputTokens: raw.Usage.CompletionTokens,
			Cost:         raw.Usage.Cost,
		}})
	}
	return out
}
