package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sse builds a response body from event payloads, ending the way the protocol
// does. Written out rather than hand-rolled per test so the framing — "data: ",
// blank lines, the [DONE] sentinel — is exercised as one thing.
func sse(events ...string) []byte {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: " + sseDone + "\n\n")
	return []byte(b.String())
}

func streamingClient(body []byte) (*OpenAI, *RecordedTransport) {
	rt := &RecordedTransport{Responses: [][]byte{body}}
	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"))
	return o, rt
}

func collect(t *testing.T, seq iter.Seq2[Chunk, error]) []Chunk {
	t.Helper()
	var out []Chunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// The whole difficulty of streaming in one test.
//
// Tool arguments arrive as fragments of a JSON document across several events,
// and no fragment is valid JSON by itself. Anything that tries to parse as it
// goes, or keys the call on its id, loses everything after the first fragment.
func TestStreamReassemblesFragmentedToolArguments(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"expr\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"240*0.15\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":52,"completion_tokens":18}}`,
	))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	acc := newAccumulator()
	for _, c := range collect(t, seq) {
		acc.add(c)
	}
	resp := acc.response()

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "calc" {
		t.Errorf("id/name lost across fragments: %+v", calls[0])
	}

	// The reassembled arguments must be valid JSON — no fragment was.
	var args struct {
		Expr string `json:"expr"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("reassembled arguments are not valid JSON: %s: %v", calls[0].Args, err)
	}
	if args.Expr != "240*0.15" {
		t.Errorf("expr = %q", args.Expr)
	}

	if resp.Stop != StopToolUse {
		t.Errorf("stop = %q", resp.Stop)
	}
	if resp.Usage.InputTokens != 52 || resp.Usage.OutputTokens != 18 {
		t.Errorf("usage = %+v; the final usage chunk was lost", resp.Usage)
	}
}

// Two calls in one turn, interleaved, with the fragments arriving out of order
// relative to each other. Index is the identity, not arrival order.
func TestStreamKeepsParallelToolCallsApart(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"calc","arguments":"{\"expr\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"fetch","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"x.html\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"1+1\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	acc := newAccumulator()
	for _, c := range collect(t, seq) {
		acc.add(c)
	}

	calls := acc.response().ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	// Order is the provider's index, not the order fragments completed in — a
	// resumed batch has to match the one it is finishing.
	if calls[0].ID != "a" || calls[1].ID != "b" {
		t.Fatalf("calls out of order: %q then %q", calls[0].ID, calls[1].ID)
	}
	if string(calls[0].Args) != `{"expr":"1+1"}` {
		t.Errorf("call a args = %s", calls[0].Args)
	}
	if string(calls[1].Args) != `{"path":"x.html"}` {
		t.Errorf("call b args = %s", calls[1].Args)
	}
}

// Streaming must produce exactly what Complete produces, or the loop's
// behaviour depends on whether somebody was watching.
func TestStreamingProviderMatchesComplete(t *testing.T) {
	text := "15% of 240 is 36."
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"content":"15% of "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"240 is "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"36."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":94,"completion_tokens":11,"cost":0.0004}}`,
	))

	var seen []string
	p := Streaming{S: o, OnDelta: func(c Chunk) {
		if c.Text != "" {
			seen = append(seen, c.Text)
		}
	}}

	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.Text() != text {
		t.Errorf("text = %q, want %q", resp.Text(), text)
	}
	if resp.Stop != StopEnd {
		t.Errorf("stop = %q", resp.Stop)
	}
	if resp.Usage.InputTokens != 94 || resp.Usage.Cost != 0.0004 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	// And the caller saw it arrive in pieces, which is the only reason to stream.
	if len(seen) != 3 || strings.Join(seen, "") != text {
		t.Errorf("deltas = %q", seen)
	}
}

// Without stream_options the provider sends no usage at all, cost silently
// becomes zero, and the cost ceiling stops meaning anything. A run reporting
// $0.0000 looks cheap, not broken, so this is asserted on the wire.
func TestStreamRequestsUsage(t *testing.T) {
	o, rt := streamingClient(sse(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, seq)

	var sent oaRequest
	if err := json.Unmarshal(rt.Bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.Stream {
		t.Error(`request did not set "stream": true`)
	}
	if sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage {
		t.Error("request did not ask for usage; cost would silently be zero")
	}
}

// [DONE] is not JSON. Decoding it is the classic way a streaming client dies on
// the very last line of an otherwise perfect response.
func TestStreamHandlesDoneSentinel(t *testing.T) {
	o, _ := streamingClient(sse(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, seq) // fails the test if [DONE] yields an error
	if len(chunks) != 1 || chunks[0].Text != "hi" {
		t.Errorf("chunks = %+v", chunks)
	}
}

// A connection closed cleanly mid-answer is an ordinary end of body. With no
// [DONE] and no finish_reason, what arrived is part of an answer and must be an
// error — it used to be accepted whole, and saved.
func TestStreamCutBeforeTheEndIsAnError(t *testing.T) {
	o, _ := streamingClient([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"IHSG closed at 7,1"}}]}` + "\n\n"))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for _, err := range seq {
		if err != nil {
			got = err
		}
	}
	if !errors.Is(got, ErrStreamCut) {
		t.Fatalf("stream cut mid-answer: err = %v, want ErrStreamCut", got)
	}

	// Through the provider the loop uses: no Response, so nothing is saved.
	o2, _ := streamingClient([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"IHSG closed at 7,1"}}]}` + "\n\n"))
	if resp, err := (&Streaming{S: o2}).Complete(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrStreamCut) {
		t.Fatalf("Complete over a cut stream: resp %+v, err %v; want ErrStreamCut", resp, err)
	}
}

// Some servers end with a finish_reason and never send [DONE]. That answer is
// whole, and refusing it would turn a working provider into a broken one.
func TestStreamFinishedWithoutDoneIsAccepted(t *testing.T) {
	body := "data: " + `{"choices":[{"index":0,"delta":{"content":"done"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	o, _ := streamingClient([]byte(body))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, seq) // fails the test on any yielded error
}

// An HTTP failure is an ordinary body, not a stream, and the caller should get
// it before iterating rather than as a first yielded error.
func TestStreamHTTPErrorIsReturnedNotYielded(t *testing.T) {
	rt := &RecordedTransport{
		Responses: [][]byte{[]byte(`{"error":{"message":"bad model"}}`)},
		Statuses:  []int{400},
	}
	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"))

	seq, err := o.Stream(context.Background(), Request{Model: "nope"})
	if err == nil {
		t.Fatal("expected an error before iteration")
	}
	if seq != nil {
		t.Error("a failed stream should not return an iterator")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error should carry the body: %v", err)
	}
}

// A mid-stream error must stop the iteration and reach the caller, not be
// silently swallowed leaving a half-assembled answer that looks complete.
func TestStreamMidStreamErrorStops(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"error":{"message":"upstream exploded"}}`,
		`{"choices":[{"index":0,"delta":{"content":" more"}}]}`,
	))

	p := Streaming{S: o}
	_, err := p.Complete(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected the mid-stream error to surface")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("err = %v", err)
	}
}

// Breaking out early must still drain and close the body, or the connection
// never returns to the pool. Asserted through the iterator's own cleanup path.
func TestStreamCleansUpOnEarlyBreak(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"content":"one"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"two"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"three"}}]}`,
	))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
		break
	}
	if n != 1 {
		t.Errorf("consumed %d chunks, want 1", n)
	}
	// If the deferred close had not run this would panic or block; reaching
	// here at all is the assertion.
}

// A garbled chunk is an error, not a skipped line. Silently dropping it would
// lose a tool-call fragment and produce arguments that cannot be parsed, far
// from where the problem was.
func TestStreamRejectsMalformedChunk(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`{not json at all`,
	))

	p := Streaming{S: o}
	if _, err := p.Complete(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("expected a decode error")
	}
}

// The accumulator is the half that has to work without a transport.
func TestAccumulatorInfersToolUseFromBlocks(t *testing.T) {
	acc := newAccumulator()
	acc.add(Chunk{ToolCall: &ToolDelta{Index: 0, ID: "x", Name: "calc", Args: `{"expr":"1"}`}})
	// A gateway that reports "stop" on a turn that carried tool calls — the
	// non-streaming path already has this rule.
	acc.add(Chunk{Stop: StopEnd})

	if got := acc.response().Stop; got != StopToolUse {
		t.Errorf("stop = %q, want tool_use when the turn produced calls", got)
	}
}

func TestAccumulatorEmptyArgumentsAreValidJSON(t *testing.T) {
	acc := newAccumulator()
	acc.add(Chunk{ToolCall: &ToolDelta{Index: 0, ID: "x", Name: "now"}})

	calls := acc.response().ToolCalls()
	if len(calls) != 1 {
		t.Fatal("no call")
	}
	var v map[string]any
	if err := json.Unmarshal(calls[0].Args, &v); err != nil {
		t.Errorf("empty arguments are not valid JSON: %s", calls[0].Args)
	}
}

// Streaming is a Provider, so the loop cannot tell the difference.
func TestStreamingSatisfiesProvider(t *testing.T) {
	var p Provider = Streaming{S: &OpenAI{}}
	if p == nil {
		t.Fatal("unreachable")
	}
}

// A Streamer whose Stream fails before any byte should surface that error
// unchanged rather than returning an empty response.
func TestStreamingSurfacesSetupError(t *testing.T) {
	boom := errors.New("no route to host")
	p := Streaming{S: failingStreamer{boom}}

	if _, err := p.Complete(context.Background(), Request{Model: "m"}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

type failingStreamer struct{ err error }

func (f failingStreamer) Stream(context.Context, Request) (iter.Seq2[Chunk, error], error) {
	return nil, f.err
}

// A provider or proxy that omits tool call IDs must not leave them empty —
// downstream resume pairs calls by ID, and empty IDs collapse into one.
// Matches fromWire's behaviour.
func TestAccumulatorSynthesizesMissingToolCallID(t *testing.T) {
	acc := newAccumulator()
	acc.add(Chunk{ToolCall: &ToolDelta{Index: 0, Name: "calc", Args: `{"expr":"1"}`}})
	acc.add(Chunk{ToolCall: &ToolDelta{Index: 1, Name: "fetch", Args: `{"path":"a"}`}})

	calls := acc.response().ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].ID != "call_synth_0" {
		t.Errorf("call 0 ID = %q, want call_synth_0", calls[0].ID)
	}
	if calls[1].ID != "call_synth_1" {
		t.Errorf("call 1 ID = %q, want call_synth_1", calls[1].ID)
	}
}

// When context is canceled during streaming, socket teardown yields generic
// read errors (e.g. closed connection). Complete must prioritize ctx.Err() so
// the caller recognizes the cancellation.
func TestStreamingHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := Streaming{S: cancelingStreamer{boom: errors.New("read tcp: use of closed network connection")}}
	_, err := p.Complete(ctx, Request{Model: "m"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want context.Canceled", err)
	}
}

type cancelingStreamer struct{ boom error }

func (c cancelingStreamer) Stream(context.Context, Request) (iter.Seq2[Chunk, error], error) {
	return func(yield func(Chunk, error) bool) {
		yield(Chunk{}, c.boom)
	}, nil
}

// Model and provider arrive once, usually on the first chunk, so the
// accumulator has to keep the first non-empty value rather than the last.
func TestAccumulatorKeepsWhoAnswered(t *testing.T) {
	a := newAccumulator()
	a.add(Chunk{Model: "openai/gpt-oss-20b", Provider: "Darkbloom"})
	a.add(Chunk{Text: "hello"})
	a.add(Chunk{Stop: StopEnd})

	got := a.response()
	if got.Model != "openai/gpt-oss-20b" || got.Provider != "Darkbloom" {
		t.Errorf("Model=%q Provider=%q, want them carried through the stream",
			got.Model, got.Provider)
	}
}

// endlessBody is a stream a provider never finishes: one real event, then
// keep-alive comments forever.
type endlessBody struct {
	head   *strings.Reader
	closed chan struct{}
}

func (b *endlessBody) Read(p []byte) (int, error) {
	if b.head.Len() > 0 {
		return b.head.Read(p)
	}
	select {
	case <-b.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return copy(p, ": keep-alive\n"), nil
}

func (b *endlessBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Stopping early must not wait for the provider to finish. Cleanup drains the
// body so the connection can be reused, and an unbounded drain of a stream the
// provider keeps writing reads until the whole answer has been generated — or,
// against this body, forever. The drain is capped; past the cap the connection
// is closed rather than reused.
func TestStreamEarlyBreakDoesNotWaitForTheRest(t *testing.T) {
	body := &endlessBody{
		head:   strings.NewReader("data: " + `{"choices":[{"index":0,"delta":{"content":"one"}}]}` + "\n\n"),
		closed: make(chan struct{}),
	}
	o := NewOpenAI("key", WithBaseURL("https://example.test/v1"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Status: "200 OK", Body: body,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Request: r}, nil
		})}))

	seq, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range seq {
			break
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		body.Close() // let the goroutine finish rather than leak it
		t.Fatal("breaking out of the stream waited for a body that never ends")
	}
	select {
	case <-body.closed:
	default:
		t.Error("the body was not closed after the early break")
	}
}

// A streamed usage chunk that says only "cost: 0" — a free model, tokens not
// counted — is still a measurement, and the accumulator must keep it.
func TestStreamKeepsAReportedZeroCost(t *testing.T) {
	o, _ := streamingClient(sse(
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"cost":0}}`,
	))
	resp, err := Streaming{S: o}.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Usage.CostReported {
		t.Errorf("usage = %+v, want the reported zero cost kept", resp.Usage)
	}
}
