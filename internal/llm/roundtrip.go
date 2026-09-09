package llm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrTransportExhausted is returned when the client makes more requests than
// the test scripted responses for. It signals a mismatch between the test's
// expectations and the code's behaviour, not a runtime failure.
var ErrTransportExhausted = errors.New("llm: recorded transport exhausted")

// RecordedTransport serves canned HTTP responses in order and records what was
// sent. Swap it into an http.Client and the provider does real JSON encoding
// and decoding against real bytes — with no network and no key.
type RecordedTransport struct {
	Responses [][]byte // response bodies, in order
	Statuses  []int    // optional, index-aligned; 0 or absent means 200
	Err       error    // if set, RoundTrip fails immediately (network-down test)

	Requests []*http.Request // what the provider sent
	Bodies   [][]byte        // ...and the bodies, already read
}

func (t *RecordedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Cancellation first, so a cancelled ctx behaves here exactly as over the wire.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}

	i := len(t.Requests)

	// Read the body NOW. Once we return, the caller has moved on and it is gone.
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("recorded transport: read request body: %w", err)
		}
		r.Body.Close()
		body = b
	}
	t.Requests = append(t.Requests, r)
	t.Bodies = append(t.Bodies, body)

	if t.Err != nil {
		return nil, t.Err
	}
	if i >= len(t.Responses) {
		return nil, fmt.Errorf("%w: call %d, only %d scripted", ErrTransportExhausted, i+1, len(t.Responses))
	}

	status := http.StatusOK
	if i < len(t.Statuses) && t.Statuses[i] != 0 {
		status = t.Statuses[i]
	}

	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(t.Responses[i])),
		Request:    r,
	}, nil
}
