package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/ginko97/ariadne/internal/eval"
	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/trace"
)

// approveListed answers an approval card yes for a tool in names and no for
// every other. An eval runs unattended: without it, a gated call fell back to
// asking on the terminal in the middle of a sweep.
func approveListed(names []string) func(context.Context, llm.ToolCall) (bool, error) {
	return func(_ context.Context, c llm.ToolCall) (bool, error) {
		return contains(names, c.Name), nil
	}
}

// cmdEval scores a task set against one or more models and prints a row each.
//
// Every model sees the same tasks, the same tools and the same scoring, which
// is the only reason the numbers can be compared at all.
func cmdEval(args []string) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	models := fs.String("models", envOr("ARIADNE_MODEL", ""), "comma-separated model ids (default depends on -base-url)")
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
	tasksPath := fs.String("tasks", "testdata/tasks.json", "task set")
	minPass := fs.Float64("min-pass-rate", 0, "exit non-zero if any model scores below this (0 = report only)")
	repeat := fs.Int("repeat", 1, "run each task this many times; it passes only if every attempt does")
	save := fs.Bool("save", false, "write each scorecard to "+historyDir+" and report regressions")
	// Present so compaction can be measured against the same task set it was
	// built beside: a budget small enough to force trimming should not collapse
	// the pass rate. That is the whole exit condition for the feature.
	budget := fs.Int("context-budget", 0, "compact the conversation past this many prompt tokens (0: never)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *models == "" {
		*models = defaultModelFor(*baseURL)
	}

	tasks, err := eval.LoadTasks(*tasksPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne eval: %v\n", err)
		return exitUsage
	}

	key, envName := apiKey(*baseURL)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne eval: no api key for %s — set %s\n", *baseURL, envName)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store := &loop.Store{Dir: runsDir}

	// One trace file per task run, opened as the agent is built. Closed when the
	// sweep ends rather than per task: a handful of open files is cheaper than
	// threading a close through the runner.
	var traces []*trace.Writer
	defer func() {
		for _, tw := range traces {
			closeTrace(tw)
		}
	}()

	taskRunID := func(model, taskID string) string {
		return fmt.Sprintf("%s_%s", newRunID(), taskID)
	}
	newAgent := func(model string, env eval.TaskEnv) *loop.Agent {
		runID, maxSteps := env.RunID, env.MaxSteps
		tw, err := trace.NewFileWriter(runsDir, runID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ariadne eval: trace: %v\n", err)
			tw = nil
		}
		traces = append(traces, tw)
		// Unrestricted: an eval measures what the agent does when it is allowed
		// to do its job, and a tool denied here would look like a model failure.
		// Never streams: an eval reads scorecards, and printing tokens for 34
		// tasks would bury them. No memory either — a sweep that remembers
		// something from task 3 is no longer measuring 34 independent tasks.
		ws := env.Workspace
		if ws == "" {
			ws = defaultWorkspace
		}
		return newAgentFor(agentOpts{
			Key: key, Model: model, BaseURL: *baseURL, RunID: runID,
			Workspace: ws,
			MaxSteps:  maxSteps, Budget: *budget, ToolTimeout: defaultToolTimeout, HTTPTimeout: defaultHTTPTimeout,
			Store: store, Trace: tw,
			ApproveFn: approveListed(env.Approve),
		})
	}

	commit := gitCommit()
	// A model scoring badly is a measurement, not a failure of the command.
	// Exit status reflects whether the sweep ran; --min-pass-rate is the gate.
	belowGate := false

	for i, model := range strings.Split(*models, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if *repeat > 1 {
			fmt.Fprintf(os.Stderr, "eval %s over %d tasks, %d times each...\n", model, len(tasks), *repeat)
		} else {
			fmt.Fprintf(os.Stderr, "eval %s over %d tasks...\n", model, len(tasks))
		}

		sc := eval.NewScorecard(model, eval.TaskSetName(*tasksPath), commit,
			eval.RunTasks(ctx, tasks, model, newAgent, taskRunID, *repeat))

		if i > 0 {
			fmt.Println()
		}
		sc.WriteTable(os.Stdout)

		if *save {
			history, err := eval.LoadHistory(historyDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ariadne eval: %v\n", err)
				return exitFail
			}
			if before, ok := eval.Previous(history, model, sc.TaskSet); ok {
				// Reported even when the pass rate is unchanged: an agent taking
				// more steps for the same answer is usually working around
				// something that broke.
				if slower := eval.StepRegressions(before, sc); len(slower) > 0 {
					fmt.Fprintf(os.Stderr, "MORE STEPS since %s: %s\n",
						before.Commit, strings.Join(slower, ", "))
				}
				if regressed := eval.Regressions(before, sc); len(regressed) > 0 {
					fmt.Fprintf(os.Stderr, "REGRESSED since %s: %s\n",
						before.Commit, strings.Join(regressed, ", "))
				}
			}
			path, err := sc.Save(historyDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ariadne eval: %v\n", err)
				return exitFail
			}
			fmt.Fprintf(os.Stderr, "saved %s\n", path)
		}

		if *minPass > 0 && sc.PassRate < *minPass {
			fmt.Fprintf(os.Stderr, "%s: pass rate %.2f below --min-pass-rate %.2f\n",
				model, sc.PassRate, *minPass)
			belowGate = true
		}
	}

	// Cancelled mid-sweep is a failure: the numbers are incomplete.
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "ariadne eval: cancelled; scorecards are incomplete")
		return exitFail
	}
	if belowGate {
		return exitFail
	}
	return exitOK
}

// gitCommit records which code produced a scorecard. Unknown is not an error:
// an eval run outside a checkout is still a valid measurement, it just cannot
// be placed in the history. A dirty working tree gets "-dirty" so uncommitted
// tweaks do not masquerade as the commit they were built on top of.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(out))
	if status, err := exec.Command("git", "status", "--porcelain").Output(); err == nil && len(strings.TrimSpace(string(status))) > 0 {
		commit += "-dirty"
	}
	return commit
}
