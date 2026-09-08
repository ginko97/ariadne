package llm

import (
	"context"
	"errors"
	"fmt"
)

// ErrFakeExhausted means the loop asked for more turns than the test scripted.
// It is a test-expectation failure, not a runtime error — assert on it with errors.Is.
var ErrFakeExhausted = errors.New("llm: fake provider exhausted")

// Fake is a Provider that returns scripted responses and records what it was asked.
// No network, no API key, no cost. Every failure-mode test in this project depends on it.
type Fake struct {
	Responses []Response
	Errs      []error // optional: Errs[i] != nil returns that error instead of Responses[i]
	Calls     []Request
}

func (f *Fake) Complete(ctx context.Context, req Request) (Response, error) {
	// Checked first so cancellation tests behave the same here as over HTTP.
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	i := len(f.Calls)
	f.Calls = append(f.Calls, req)

	if i < len(f.Errs) && f.Errs[i] != nil {
		return Response{}, f.Errs[i]
	}
	if i >= len(f.Responses) {
		return Response{}, fmt.Errorf("%w: call %d, only %d scripted", ErrFakeExhausted, i+1, len(f.Responses))
	}
	return f.Responses[i], nil
}
