// Command ariadne runs an agent.
//
// A run is a job, not a chat session: it has an id, a step ceiling, a cost
// ceiling, and a checkpoint it can be resumed from.
//
//	ariadne run "What is 15% of 240?"
//
// The answer goes to stdout and everything else to stderr, so
//
//	ariadne run "..." > answer.txt
//
// leaves you with the answer and nothing else.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/dotenv"
	"github.com/ginko97/ariadne/internal/eval"
	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/tool"
	"github.com/ginko97/ariadne/internal/trace"
)

const (
	defaultModel   = "gemini-2.5-flash"
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	runsDir        = "runs"
	historyDir     = "eval/history"
)

// Exit codes: 0 the run succeeded, 1 it failed, 2 the command line was wrong.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	// Best effort: a missing .env is not an error, the env var may already be set.
	_ = dotenv.Load()

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "resume":
		os.Exit(cmdResume(os.Args[2:]))
	case "eval":
		os.Exit(cmdEval(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "ariadne: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ariadne — an agent runtime where a run is a job

usage:
  ariadne run    [flags] <task>
  ariadne resume [flags] <run-id>
  ariadne eval   [flags]                 score a task set, one row per model

flags:
  -model          model id                   (env ARIADNE_MODEL)
  -base-url       OpenAI-compatible endpoint (env ARIADNE_BASE_URL)
  -max-steps      ceiling on loop iterations (default 10)

eval flags:
  -models         comma-separated model ids  (default: ARIADNE_MODEL)
  -tasks          path to task set           (default testdata/tasks.json)
  -min-pass-rate  exit non-zero if any model scores below this
  -save           write scorecard to eval/history and report regressions

environment:
  ARIADNE_API_KEY   api key; falls back to GEMINI_API_KEY, then OPENROUTER_API_KEY
                    .env in the repo root is read if present
`)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	model := fs.String("model", envOr("ARIADNE_MODEL", defaultModel), "model id")
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
	maxSteps := fs.Int("max-steps", 10, "ceiling on loop iterations")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, "ariadne run: a task is required")
		return exitUsage
	}

	key, envName := apiKey(*baseURL)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne run: no api key for %s — set %s in the environment or .env\n", *baseURL, envName)
		return exitUsage
	}

	// Ctrl-C cancels the run rather than killing the process outright, so the
	// loop's guards see a cancelled context and can stop cleanly — which is also
	// what gives the checkpoint a chance to be the last thing written.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store := &loop.Store{Dir: runsDir}
	state := loop.NewState(newRunID(), task)

	tw, err := trace.NewFileWriter(runsDir, state.RunID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne run: %v\n", err)
		return exitFail
	}
	defer closeTrace(tw)

	agent := newAgentFor(key, *model, *baseURL, *maxSteps, store, tw)
	state.BaseURL = *baseURL
	fmt.Fprintf(os.Stderr, "run %s  model=%s\n", state.RunID, *model)

	return execute(ctx, agent, state)
}

// cmdResume continues an interrupted run from its checkpoint.
//
// Run takes a *State precisely so this is load-and-call rather than a second
// code path: the loop cannot tell a resumed run from a fresh one.
func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	baseURL := fs.String("base-url", "", "OpenAI-compatible endpoint (defaults to endpoint from checkpoint)")
	maxSteps := fs.Int("max-steps", 10, "ceiling on loop iterations")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(os.Stderr, "ariadne resume: a run id is required")
		return exitUsage
	}
	if len(fs.Args()) > 1 {
		fmt.Fprintf(os.Stderr, "ariadne resume: unexpected arguments %v (flags must come before run id)\n", fs.Args()[1:])
		return exitUsage
	}
	runID := fs.Args()[0]

	store := &loop.Store{Dir: runsDir}
	state, err := store.Load(runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitFail
	}

	endpoint := *baseURL
	if endpoint == "" {
		if state.BaseURL != "" {
			endpoint = state.BaseURL
		} else {
			endpoint = envOr("ARIADNE_BASE_URL", defaultBaseURL)
		}
	}

	key, envName := apiKey(endpoint)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne resume: no api key for %s — set %s in the environment or .env\n", endpoint, envName)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tw, err := trace.NewFileWriter(runsDir, state.RunID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitFail
	}
	defer closeTrace(tw)

	// The checkpoint's model wins: a job that finishes on a different model
	// than it started on is a different job. The checkpoint's endpoint wins
	// unless explicitly overridden on the command line.
	agent := newAgentFor(key, state.Model, endpoint, *maxSteps, store, tw)

	fmt.Fprintf(os.Stderr, "resume %s  model=%s  from step %d (%d messages)\n",
		state.RunID, state.Model, state.Steps, len(state.Messages))

	return execute(ctx, agent, state)
}

func newAgentFor(key, model, baseURL string, maxSteps int, store *loop.Store, tw *trace.Writer) *loop.Agent {
	reg := tool.New(tool.Calc{})
	return &loop.Agent{
		Trace:      tw.Emit,
		Provider:   llm.NewOpenAI(key, llm.WithBaseURL(baseURL)),
		Model:      model,
		BaseURL:    baseURL,
		Tools:      reg.Defs(),
		RunTool:    reg.Call,
		Checkpoint: store.Save,
		MaxSteps:   maxSteps,
		// MaxCost stays 0 (unlimited) until Price is a per-model table —
		// a ceiling with no prices behind it would be theatre.
	}
}

// execute runs the agent and reports. Shared by run and resume so the two
// cannot drift in how they print or what they exit with.
func execute(ctx context.Context, agent *loop.Agent, state *loop.State) int {
	started := time.Now()
	answer, err := agent.Run(ctx, state)
	elapsed := time.Since(started).Round(time.Millisecond)

	if err != nil {
		fmt.Fprintf(os.Stderr, "run %s failed after %d steps in %s: %v\n",
			state.RunID, state.Steps, elapsed, err)
		switch {
		case errors.Is(err, loop.ErrStepLimit):
			fmt.Fprintf(os.Stderr, "hint: ariadne resume -max-steps 20 %s\n", state.RunID)
		case errors.Is(err, context.Canceled):
			fmt.Fprintf(os.Stderr, "cancelled; resume with: ariadne resume %s\n", state.RunID)
		}
		return exitFail
	}

	fmt.Println(answer)
	fmt.Fprintf(os.Stderr, "run %s  steps=%d  cost=%.4f  %s\n",
		state.RunID, state.Steps, state.Cost, elapsed)
	return exitOK
}

// newRunID is sortable by time and unique enough for a single machine.
// It becomes the directory name under runs/ once checkpoints land.
func newRunID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run_%s_%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// apiKey resolves the key for one endpoint. ARIADNE_API_KEY always wins;
// otherwise the host decides.
//
// Deciding by host matters as soon as .env holds more than one provider's key:
// a fixed precedence order would send the Gemini key to OpenRouter and produce
// a 401 that reads as "bad key" rather than "wrong key".
func apiKey(baseURL string) (key, envName string) {
	if v := os.Getenv("ARIADNE_API_KEY"); v != "" {
		return v, "ARIADNE_API_KEY"
	}

	name := ""
	switch {
	case strings.Contains(baseURL, "openrouter.ai"):
		name = "OPENROUTER_API_KEY"
	case strings.Contains(baseURL, "googleapis.com"):
		name = "GEMINI_API_KEY"
	case strings.Contains(baseURL, "x.ai"):
		name = "XAI_API_KEY"
	}
	if name != "" {
		return os.Getenv(name), name
	}

	// Unrecognised host: take whatever is set, but say which one was used.
	for _, k := range []string{"OPENROUTER_API_KEY", "GEMINI_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			return v, k
		}
	}
	return "", "ARIADNE_API_KEY"
}

// cmdEval scores a task set against one or more models and prints a row each.
//
// Every model sees the same tasks, the same tools and the same scoring, which
// is the only reason the numbers can be compared at all.
func cmdEval(args []string) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	models := fs.String("models", envOr("ARIADNE_MODEL", defaultModel), "comma-separated model ids")
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
	tasksPath := fs.String("tasks", "testdata/tasks.json", "task set")
	minPass := fs.Float64("min-pass-rate", 0, "exit non-zero if any model scores below this (0 = report only)")
	save := fs.Bool("save", false, "write each scorecard to "+historyDir+" and report regressions")

	if err := fs.Parse(args); err != nil {
		return exitUsage
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
	newAgent := func(model, runID string, maxSteps int) *loop.Agent {
		tw, err := trace.NewFileWriter(runsDir, runID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ariadne eval: trace: %v\n", err)
			tw = nil
		}
		traces = append(traces, tw)
		return newAgentFor(key, model, *baseURL, maxSteps, store, tw)
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
		fmt.Fprintf(os.Stderr, "eval %s over %d tasks...\n", model, len(tasks))

		sc := eval.NewScorecard(model, commit,
			eval.RunTasks(ctx, tasks, model, newAgent, taskRunID))

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
			if before, ok := eval.Previous(history, model); ok {
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

// closeTrace reports a broken trace without failing anything: a run that
// finished is still a run, but a trace that silently stopped recording would
// otherwise look like a run that never did those things.
func closeTrace(tw *trace.Writer) {
	if err := tw.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: trace incomplete: %v\n", err)
	}
}
