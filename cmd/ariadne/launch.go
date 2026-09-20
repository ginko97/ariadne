package main

// startUIInstead decides whether a bare `ariadne` should open the browser
// rather than print usage.
//
// Split from launchedFromExplorer so the rule is testable on any platform:
// the Win32 call cannot be made to return 1 from inside `go test`, which runs
// under a test binary that shares the console with the shell.
//
// The rule is deliberately narrow. Only a launch with no arguments at all
// qualifies, so `ariadne --help` from Explorer still prints, and anything that
// passes a subcommand is unaffected. A wrong guess here costs a server nobody
// asked for, which is why it is not extended to "no arguments and no terminal"
// — a piped or redirected run has no terminal either, and starting a web
// server inside somebody's shell script would be worse than a usage message.
func startUIInstead(fromExplorer bool) bool {
	return fromExplorer
}
