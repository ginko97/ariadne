// Package trace records what a run did, one JSON object per line.
//
// A checkpoint says where a run ended up; a trace says how it got there. Error
// analysis needs the second — "the model answered wrongly" is not a finding,
// "the model called calc with the operands reversed on step 2" is.
//
// The format is JSONL and the struct is flat on purpose: one line per event
// means grep and jq work without a parser, and a flat shape means
// `jq 'select(.type=="tool_call") | .tool'` needs no knowledge of the schema.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event kinds. Strings rather than ints so a trace stays readable in a terminal.
const (
	KindRunStart   = "run_start"
	KindRequest    = "request"
	KindResponse   = "response"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	KindRunEnd     = "run_end"
	// KindToolDenied is a call the allow-list refused. It is deliberately not a
	// tool_result with is_error set: "the agent tried to do something it was not
	// permitted to do" is the line you want to be able to grep for on its own,
	// and it reads very differently from a tool that ran and failed.
	KindToolDenied = "tool_denied"
	// KindApproval records that a human was asked about a call, and what they
	// said. Both answers are recorded: an audit that only shows refusals cannot
	// answer "who let this happen", which is the question actually asked after
	// something goes wrong.
	KindApproval = "approval"
	// KindRetry is a request the provider is about to send again after being
	// rate limited. It carries the wait in LatencyMS — time this run spent not
	// working, which is otherwise indistinguishable from a slow model, because
	// the loop measures latency around the whole call. It has no Step: the
	// provider does not know what a step is. Sequence puts it between the
	// request and the response it belongs to.
	KindRetry = "retry"
	// KindCompact is the loop dropping the oldest turns to stay inside the
	// context budget. It is the only event that records something being
	// *removed* from the conversation, which makes the trace the only complete
	// account of a long run: the checkpoint holds what the agent still knows,
	// this holds what it used to.
	KindCompact = "compact"
	// KindToolTimeout is a tool the runtime stopped waiting for. Its own kind
	// rather than a failed result, because the call was *abandoned* rather than
	// finished: it may still be running, and whatever it does may still happen.
	// "We gave up on it" and "it failed" are different facts and only one of
	// them means nothing happened.
	KindToolTimeout = "tool_timeout"
)

// Event is one thing that happened. Fields are shared across kinds and omitted
// when empty, which keeps lines short without needing a union type.
type Event struct {
	RunID string    `json:"run_id"`
	Seq   int       `json:"seq"`
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Step  int       `json:"step,omitempty"`

	Model     string  `json:"model,omitempty"`
	Stop      string  `json:"stop,omitempty"`
	InTokens  int     `json:"input_tokens,omitempty"`
	OutTokens int     `json:"output_tokens,omitempty"`
	Cost      float64 `json:"cost_usd,omitempty"`
	LatencyMS int64   `json:"latency_ms,omitempty"`
	Messages  int     `json:"messages,omitempty"`

	CallID  string          `json:"call_id,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Content string          `json:"content,omitempty"`
	IsError bool            `json:"is_error,omitempty"`

	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// Writer appends events to a stream.
//
// Emit never returns an error and never blocks a run: tracing is observability,
// and losing a line is not worth failing work that is otherwise fine. That is
// the opposite of the checkpoint contract, where a failed write means the run
// can no longer be resumed and must stop. The first write error is kept and
// reported by Close, so a silently broken trace is still discoverable.
type Writer struct {
	mu       sync.Mutex
	w        io.Writer
	closer   io.Closer
	runID    string
	seq      int
	firstErr error
}

// NewWriter traces to an arbitrary stream. Useful in tests.
func NewWriter(w io.Writer, runID string) *Writer {
	return &Writer{w: w, runID: runID}
}

// NewFileWriter traces to <dir>/<runID>/trace.jsonl, appending if it exists.
//
// Appending matters: a resumed run continues the same trace, so one file is the
// whole history of a run across every process that worked on it.
func NewFileWriter(dir, runID string) (*Writer, error) {
	if strings.Contains(runID, "..") || strings.ContainsAny(runID, `/\`) {
		return nil, fmt.Errorf("trace: refusing run id with path traversal %q", runID)
	}
	runDir := filepath.Join(dir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("trace: create dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(runDir, "trace.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("trace: open: %w", err)
	}
	return &Writer{w: f, closer: f, runID: runID}, nil
}

// Emit writes one event, filling in run id, sequence and timestamp.
func (w *Writer) Emit(e Event) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.seq++
	e.RunID, e.Seq, e.At = w.runID, w.seq, time.Now().UTC()

	line, err := json.Marshal(e)
	if err != nil {
		w.keep(fmt.Errorf("trace: encode event %d: %w", e.Seq, err))
		return
	}
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		w.keep(fmt.Errorf("trace: write event %d: %w", e.Seq, err))
	}
}

func (w *Writer) keep(err error) {
	if w.firstErr == nil {
		w.firstErr = err
	}
}

// Err reports the first write failure, if any. A trace that silently stopped
// recording is worse than one that never started.
func (w *Writer) Err() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firstErr
}

// Close flushes the underlying file and reports the first write error.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closer != nil {
		if err := w.closer.Close(); err != nil && w.firstErr == nil {
			w.firstErr = err
		}
	}
	return w.firstErr
}

// Read parses a trace file. Used by analysis tooling and by tests.
func Read(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("trace: read: %w", err)
	}
	return Decode(data)
}

// Decode parses JSONL. A malformed line names its own number: a trace is
// append-only and a truncated final line is normal after a crash, so the caller
// can decide whether to tolerate it.
func Decode(data []byte) ([]Event, error) {
	var out []Event
	dec := json.NewDecoder(bytes.NewReader(data))
	for i := 1; ; i++ {
		var e Event
		err := dec.Decode(&e)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("trace: line %d: %w", i, err)
		}
		out = append(out, e)
	}
}
