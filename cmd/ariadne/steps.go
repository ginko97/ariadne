package main

import (
	"flag"
	"fmt"

	"github.com/ginko97/ariadne/internal/loop"
)

// defaultMaxSteps is a turn's ceiling on loop iterations when -max-steps is
// not given.
const defaultMaxSteps = 10

// briefMaxSteps is the ceiling instead in a conversation started from a
// brief. A research brief reads a page or two per step: the QRIS comparison
// (run_20260923T200748_3e40bc) used exactly the 10 a turn allowed, and one
// more retry would have ended it at "step limit exceeded" before the report
// was written. A brief is work the operator wrote down and started on
// purpose, so it gets room; a typed message keeps the tighter default. Every
// gated call still asks. The ceiling is also the most a turn can spend: 25
// model calls instead of 10, and nothing else caps cost.
const briefMaxSteps = 25

var maxStepsHelp = fmt.Sprintf("ceiling on loop iterations, per turn (default %d; %d in a conversation started from a brief)",
	defaultMaxSteps, briefMaxSteps)

// maxStepsFor is the step ceiling for a turn of state: -max-steps when it was
// given, briefMaxSteps for a brief conversation, the flag's default otherwise.
// state may be nil (a chat before its first message).
func maxStepsFor(fs *flag.FlagSet, flagValue int, state *loop.State) int {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "max-steps" {
			explicit = true
		}
	})
	if !explicit && state != nil && state.Brief != "" {
		return briefMaxSteps
	}
	return flagValue
}
