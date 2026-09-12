// Command ariadne runs an agent.
//
// A run is a job, not a chat session: it has an id, a step ceiling, a cost
// ceiling, and — from week 5 — a checkpoint it can be resumed from.
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
	"os/signal"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/dotenv"
	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/tool"
)

const (
	defaultModel   = "gemini-2.5-flash"
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	runsDir        = "runs"
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

flags:
  -model      model id                  (env ARIADNE_MODEL)
  -base-url   OpenAI-compatible endpoint (env ARIADNE_BASE_URL)
  -max-steps  ceiling on loop iterations (default 10)

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
	// loop's guards see a cancelled context and can stop cleanly. In week 5 this
	// is also what gives the checkpoint a chance to be the last thing written.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store := &loop.Store{Dir: runsDir}
	agent := newAgent(key, *model, *baseURL, *maxSteps, store)

	state := loop.NewState(newRunID(), task)
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
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
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

	key, envName := apiKey(*baseURL)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne resume: no api key for %s — set %s in the environment or .env\n", *baseURL, envName)
		return exitUsage
	}

	store := &loop.Store{Dir: runsDir}
	state, err := store.Load(runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitFail
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The checkpoint's model wins: a job that finishes on a different model
	// than it started on is a different job.
	agent := newAgent(key, state.Model, *baseURL, *maxSteps, store)

	fmt.Fprintf(os.Stderr, "resume %s  model=%s  from step %d (%d messages)\n",
		state.RunID, state.Model, state.Steps, len(state.Messages))

	return execute(ctx, agent, state)
}

func newAgent(key, model, baseURL string, maxSteps int, store *loop.Store) *loop.Agent {
	reg := tool.New(tool.Calc{})
	return &loop.Agent{
		Provider:   llm.NewOpenAI(key, llm.WithBaseURL(baseURL)),
		Model:      model,
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
