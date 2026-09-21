package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/tool"
)

// approveOnTerminal asks the operator before a gated call runs.
//
// This is the one place the runtime blocks on a human, and it is why -approve
// is opt-in. A run is a job: something you can schedule, walk away from, and
// resume after a crash. A job that stops and waits for someone to type is none
// of those things, so the gate exists for the calls where that trade is worth
// making and not as a default.
//
// With stdin not a terminal there is nobody to ask, and the answer is no. That
// is the whole reason this fails closed rather than assuming consent: the
// unattended case is exactly the one where a wrong guess is unrecoverable.
// Unanswered prompts time out after 5 minutes (matching the browser UI) and
// deny the call; a cancelled context (e.g. Ctrl-C) unblocks immediately.
func approveOnTerminal(in *os.File, workspace string) func(context.Context, llm.ToolCall) (bool, error) {
	return approveOnTerminalReader(in, bufio.NewReader(in), workspace)
}

func approveOnTerminalReader(in *os.File, reader *bufio.Reader, workspace string) func(context.Context, llm.ToolCall) (bool, error) {
	return func(ctx context.Context, c llm.ToolCall) (bool, error) {
		if !isTerminal(in) {
			// A gated built-in names the flag that would let it run
			// unattended, because "denied" with no way forward reads as a bug
			// in a script that used to work.
			hint := ""
			if tool.Gated(c.Name) {
				hint = fmt.Sprintf(" (-trust %s to run it without asking)", c.Name)
			}
			fmt.Fprintf(os.Stderr, "denied %s: approval required and no terminal to ask%s\n", c.Name, hint)
			return false, nil
		}
		return approveFromReader(ctx, reader, os.Stderr, c, workspace)
	}
}

// previewMark is how a preview line's kind reads in a terminal. The browser
// styles the same kinds with colour instead.
func previewMark(kind string) string {
	switch kind {
	case "added":
		return "+ "
	case "removed":
		return "- "
	case "warn":
		return "! "
	}
	return "  "
}

// terminalApprovalTimeout bounds how long the terminal waits for operator approval.
// Matches the browser UI timeout (5 minutes) so unanswered prompts fail closed.
const terminalApprovalTimeout = 5 * time.Minute

// approveFromReader prompts the operator on out and reads an approval answer, with
// a 5-minute timeout matching the browser interface.
func approveFromReader(ctx context.Context, reader *bufio.Reader, out io.Writer, c llm.ToolCall, workspace string) (bool, error) {
	return approveFromReaderWithTimeout(ctx, reader, out, c, workspace, terminalApprovalTimeout)
}

// approveFromReaderWithTimeout prompts the operator on out and bounds the wait by timeout.
func approveFromReaderWithTimeout(ctx context.Context, reader *bufio.Reader, out io.Writer, c llm.ToolCall, workspace string, timeout time.Duration) (bool, error) {
	// What the call does, not the JSON it arrived as: the arguments of an edit
	// are three escaped strings on one line, and a gate nobody reads is not a
	// gate. tool.Preview reads the file to say what would change; it is shown
	// here and discarded, never sent to the model.
	fmt.Fprintf(out, "\napprove %s?\n", c.Name)
	for _, l := range tool.Preview(c.Name, c.Args, workspace) {
		fmt.Fprintf(out, "  %s%s\n", previewMark(l.Kind), l.Text)
	}
	fmt.Fprint(out, "[y/N] ")

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timeoutCh:
		fmt.Fprintln(out, "\napproval timed out; denied")
		return false, nil
	case res := <-ch:
		if res.err != nil {
			// EOF on a terminal means the operator closed the input rather than
			// answering. Not an answer, so not a yes.
			fmt.Fprintln(out, "no answer; denied")
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(res.line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}
