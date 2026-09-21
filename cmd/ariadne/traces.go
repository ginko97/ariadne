package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ginko97/ariadne/internal/trace"
)

// cmdTraces searches the JSONL traces every run writes.
//
// A subcommand rather than a tool the agent can call. A trace holds every byte
// a run ever saw — fetched documents included, and demonstrably credentials —
// so search across runs would be a read channel from any run into any other.
// That is a better exfiltration surface than the attack that already worked,
// because it needs no injection at all. See docs/injection-postmortem.md.
func cmdTraces(args []string) int {
	fs := flag.NewFlagSet("traces", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	run := fs.String("run", "", "limit to runs whose id contains this")
	kinds := fs.String("kind", "", "comma-separated event kinds (run_start, request, response, tool_call, tool_result, tool_denied, tool_timeout, approval, retry, compact, run_end)")
	tools := fs.String("tool", "", "comma-separated tool names")
	errsOnly := fs.Bool("errors", false, "only events that record something going wrong")
	stats := fs.Bool("stats", false, "aggregate instead of listing")
	limit := fs.Int("limit", 50, "maximum events to print (0: all)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// Go's flag package stops at the first non-flag argument, so a flag typed
	// after the query silently becomes part of the query and the search returns
	// "no matching events" — which reads exactly like a genuine empty result.
	// Caught here rather than left to be discovered.
	for _, a := range fs.Args() {
		// Name only: "-limit", "--limit" and "-limit=4" are the same mistake, and
		// the last form is the one that slipped through a first version of this.
		// Looked up rather than pattern-matched, so a search for text that
		// merely starts with a dash — "-1200", which is in the fixtures — is
		// still a search and not an error.
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "--"), "-"), "=")
		if strings.HasPrefix(a, "-") && fs.Lookup(name) != nil {
			fmt.Fprintf(os.Stderr,
				"ariadne traces: %q looks like a flag but came after the query; flags must come first\n", a)
			return exitUsage
		}
	}

	q := trace.Query{
		RunID:  *run,
		Kinds:  splitList(*kinds),
		Tools:  splitList(*tools),
		Text:   strings.TrimSpace(strings.Join(fs.Args(), " ")),
		Errors: *errsOnly,
	}

	if *stats {
		return printStats(q)
	}

	matches, err := trace.Search(runsDir, q, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne traces: %v\n", err)
		return exitFail
	}
	if len(matches) == 0 {
		fmt.Fprintln(os.Stderr, "no matching events")
		return exitOK
	}

	for _, m := range matches {
		fmt.Printf("%s  %-12s %s\n", shortID(m.RunID), m.Event.Kind, describe(m.Event))
	}
	fmt.Fprintf(os.Stderr, "\n%d events\n", len(matches))
	return exitOK
}

func printStats(q trace.Query) int {
	st, err := trace.Summarise(runsDir, q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne traces: %v\n", err)
		return exitFail
	}

	fmt.Printf("runs      %d\n", st.Runs)
	fmt.Printf("events    %d\n", st.Events)
	fmt.Printf("steps     %d\n", st.Steps)
	fmt.Printf("cost      %s\n", costText(st.Cost, st.Unpriced > 0, 5))
	if st.Unpriced > 0 {
		fmt.Printf("unpriced  %d responses (the provider reported no cost)\n", st.Unpriced)
	}
	if st.Retried > 0 {
		// Called out because the loop times the whole provider call, so this
		// much of the elapsed time was not work.
		fmt.Printf("retries   %d (%.1fs waiting on rate limits)\n",
			st.Retried, float64(st.RetryMS)/1000)
	}
	if st.Denied > 0 {
		fmt.Printf("denied    %d tool calls refused\n", st.Denied)
	}

	if len(st.ByKind) > 0 {
		fmt.Println("\nby kind")
		for _, k := range sortedKeys(st.ByKind) {
			fmt.Printf("  %-14s %d\n", k, st.ByKind[k])
		}
	}
	if len(st.ByTool) > 0 {
		fmt.Println("\nby tool")
		for _, k := range sortedKeys(st.ByTool) {
			fmt.Printf("  %-14s %d\n", k, st.ByTool[k])
		}
	}
	if st.Malformed > 0 {
		fmt.Printf("\n%d unparseable lines skipped\n", st.Malformed)
	}
	if len(st.Failed) > 0 {
		fmt.Printf("\nfailed runs (%d)\n", len(st.Failed))
		for _, r := range st.Failed {
			fmt.Printf("  %s\n", shortID(r))
		}
	}
	if len(st.Incomplete) > 0 {
		// Killed rather than failed: no run_end at all, so these never appear
		// above. For a runtime that claims to survive kill -9, this is the
		// list to check resume against.
		fmt.Printf("\nincomplete runs (%d) — started, never ended\n", len(st.Incomplete))
		for _, r := range st.Incomplete {
			fmt.Printf("  %s\n", shortID(r))
		}
	}
	return exitOK
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shortID trims the date prefix a run id carries, keeping the part that
// distinguishes it. The full id is still what resume takes.
func shortID(id string) string {
	if i := strings.LastIndex(id, "_"); i > 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// describe renders the interesting half of an event on one line.
//
// Per kind rather than a generic dump: a tool_call wants its arguments, a
// response wants tokens and cost, a denial wants the reason. A single format
// would show mostly empty fields and bury the one that matters.
func describe(e trace.Event) string {
	switch e.Kind {
	case trace.KindRequest:
		return fmt.Sprintf("model=%-20s msgs=%d", e.Model, e.Messages)
	case trace.KindToolCall:
		return fmt.Sprintf("%-10s %s", e.Tool, clip(string(e.Args), 90))
	case trace.KindToolResult, trace.KindToolDenied, trace.KindToolTimeout:
		flag := ""
		if e.IsError {
			flag = "! "
		}
		return fmt.Sprintf("%-10s %s%s", e.Tool, flag, clip(e.Content, 90))
	case trace.KindApproval:
		return fmt.Sprintf("%-10s %s", e.Tool, e.Content)
	case trace.KindResponse:
		s := fmt.Sprintf("stop=%-9s in=%-6d out=%-5d %s", e.Stop, e.InTokens, e.OutTokens, costText(e.Cost, e.CostUnknown, 6))
		if e.Text != "" {
			s += "  " + clip(e.Text, 70)
		}
		if e.Error != "" {
			s += "  ! " + clip(e.Error, 70)
		}
		return s
	case trace.KindRetry, trace.KindCompact:
		return clip(e.Content, 100)
	case trace.KindRunStart:
		return clip(e.Text, 100)
	case trace.KindRunEnd:
		if e.Error != "" {
			return fmt.Sprintf("! %s", clip(e.Error, 100))
		}
		return fmt.Sprintf("ok  steps=%d  %s", e.Step, costText(e.Cost, e.CostUnknown, 5))
	default:
		return clip(e.Text+e.Content, 100)
	}
}

// costText prints a cost so that unmeasured never reads as free. A provider
// that reports no cost, with no price to fall back on, leaves zero in the
// total; "$0.0000" would claim the run cost nothing. Nothing measured prints
// "unknown", and a total that is missing some steps prints as a lower bound.
func costText(usd float64, unknown bool, digits int) string {
	switch {
	case unknown && usd == 0:
		return "unknown"
	case unknown:
		return fmt.Sprintf(">=$%.*f", digits, usd)
	default:
		return fmt.Sprintf("$%.*f", digits, usd)
	}
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
