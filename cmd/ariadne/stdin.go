package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// errLineTimeout is what lineSource.next returns when nothing arrived in time.
var errLineTimeout = errors.New("no input before the deadline")

// lineSource is the only reader of an input stream: one goroutine reads lines
// and hands each to whichever caller asks next.
//
// It replaces a goroutine per read, which is how v0.5.4 gave the terminal
// approval prompt a timeout. os.Stdin has no read deadline, so a read that
// times out cannot be cancelled — its goroutine stayed blocked on a
// bufio.Reader shared with the chat REPL and every later prompt. The next line
// the operator typed went to that orphan and was thrown away: an answer to
// prompt 2 read by prompt 1's leftover, prompt 2 left waiting for whatever came
// after it, and two goroutines racing on a reader that is not safe for that.
//
// Here there is exactly one reader. A caller that stops waiting — a timeout,
// a cancelled context — takes nothing with it: the next line stays with the
// goroutine until somebody asks for it, which is what typing into a terminal
// is supposed to mean.
//
// The goroutine lives as long as the stream. For os.Stdin that is the process,
// and there is one of it (see stdinSource); it is not a leak per call.
type lineSource struct {
	start sync.Once
	r     *bufio.Reader
	ch    chan lineResult
}

type lineResult struct {
	line string
	err  error
}

func newLineSource(r io.Reader) *lineSource {
	return &lineSource{r: bufio.NewReader(r), ch: make(chan lineResult)}
}

// run reads until the stream ends. The channel is unbuffered, so a line read
// ahead of any caller waits here rather than in a buffer nobody drains.
func (s *lineSource) run() {
	for {
		line, err := s.r.ReadString('\n')
		s.ch <- lineResult{line: line, err: err}
		if err != nil {
			close(s.ch)
			return
		}
	}
}

// next returns the next line, with bufio.Reader.ReadString's contract: a final
// line without a newline comes back with io.EOF, and every call after the end
// returns ("", io.EOF). A timeout of zero waits for as long as ctx allows.
func (s *lineSource) next(ctx context.Context, timeout time.Duration) (string, error) {
	s.start.Do(func() { go s.run() })

	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-deadline:
		return "", errLineTimeout
	case res, ok := <-s.ch:
		if !ok {
			return "", io.EOF
		}
		return res.line, res.err
	}
}

var (
	stdinOnce sync.Once
	stdinSrc  *lineSource
)

// stdinSource is the process's one reader of os.Stdin.
//
// One per process, not one per caller: two bufio.Readers on the same stream
// each buffer ahead, so whichever reads first takes bytes the other was
// waiting for. Before this, every agent built without an explicit approver
// made its own — harmless while a command built one agent, and a lost
// keystroke the day one built two.
func stdinSource() *lineSource {
	stdinOnce.Do(func() { stdinSrc = newLineSource(os.Stdin) })
	return stdinSrc
}
