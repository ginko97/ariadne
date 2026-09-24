package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/trace"
)

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	model := fs.String("model", envOr("ARIADNE_MODEL", ""), "model id (default depends on -base-url)")
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
	maxSteps := fs.Int("max-steps", defaultMaxSteps, maxStepsHelp)
	allow := fs.String("allow", "", "comma-separated tools this run may call (default: all)")
	workspace := fs.String("workspace", defaultWorkspace, "directory the file tools are confined to")
	mcpConfig := fs.String("mcp-config", envOr("ARIADNE_MCP_CONFIG", ""), "JSON file listing MCP servers to start")
	approve := fs.String("approve", "", "comma-separated tools that need a yes on the terminal before each call")
	trust := fs.String("trust", "", "MCP tools, or a gated built-in (web_fetch, edit_file, write_file), that run without approval; every other one asks first")
	allowExec := fs.Bool("exec", false, "offer the exec tool: runs a program in the workspace, and every call asks first")
	budget := fs.Int("context-budget", 0, "compact the conversation when the prompt exceeds this many tokens (0: never)")
	stream := fs.Bool("stream", false, "print tokens and tool calls as they arrive")
	remember := fs.Bool("remember", false, "let the run read and append to `MEMORY.md`")
	toolTimeout := fs.Duration("tool-timeout", defaultToolTimeout, "abandon a tool call that runs longer than this (0: never)")
	httpTimeout := fs.Duration("http-timeout", defaultHTTPTimeout, "bound one provider request, body included (0: only the context)")
	taskFile := fs.String("task", "", "markdown file containing the task (shows brief before running)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *model == "" {
		*model = defaultModelFor(*baseURL)
	}

	var task string
	var briefPath string
	if *taskFile != "" {
		if len(fs.Args()) > 0 {
			fmt.Fprintln(os.Stderr, "ariadne run: cannot provide both -task <file> and positional task text")
			return exitUsage
		}
		t, code := readTaskFile("ariadne run", *taskFile)
		if code != exitOK {
			return code
		}
		task = t
		briefPath = *taskFile
		fmt.Fprintf(os.Stderr, "task from %s:\n%s\n\n", briefPath, task)
	} else {
		task = strings.TrimSpace(strings.Join(fs.Args(), " "))
		if task == "" {
			fmt.Fprintln(os.Stderr, "ariadne run: a task is required")
			return exitUsage
		}
	}

	rememberFor := ""
	if *remember {
		rememberFor = "validate"
	}

	// An explicit allow-list is the operator's sentence, and this project
	// refuses to widen one everywhere else — resume can narrow a grant and
	// never broaden it. Adding remember for convenience would be the same
	// widening with a friendlier face, so it is an error instead. Silently
	// leaving it out is worse than either: the tool would be offered and then
	// refused at the loop, leaving the read side on with a dead write side.
	if *remember && len(splitList(*allow)) > 0 && !contains(splitList(*allow), "remember") {
		fmt.Fprintln(os.Stderr,
			"ariadne run: -remember with -allow needs remember in the list")
		return exitUsage
	}

	key, envName := apiKey(*baseURL)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne run: no api key for %s — run `ariadne setup`, or set %s\n", *baseURL, envName)
		return exitUsage
	}

	// Ctrl-C cancels the run rather than killing the process outright, so the
	// loop's guards see a cancelled context and can stop cleanly — which is also
	// what gives the checkpoint a chance to be the last thing written.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	mcpTools, closeMCP, err := connectMCP(ctx, *mcpConfig, rememberFor, *workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne run: %v\n", err)
		return exitUsage
	}
	defer closeMCP()
	// After connecting, not before: -allow and -approve can name a tool an MCP
	// server provides, and checking against the local registry alone made an
	// MCP tool impossible to gate or grant.
	if err := checkNames(withRemote(newRegistry(rememberFor, *workspace, *allowExec).Defs(), mcpTools), splitList(*allow), splitList(*approve)); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne run: %v\n", err)
		return exitUsage
	}
	gated, err := gateMCP(splitList(*approve), splitList(*trust), mcpTools)
	trusted := splitList(*trust)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne run: %v\n", err)
		return exitUsage
	}

	store := &loop.Store{Dir: runsDir}
	state := loop.NewState(newRunID(), task)
	state.Brief = briefPath

	tw, err := trace.NewFileWriter(runsDir, state.RunID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne run: %v\n", err)
		return exitFail
	}
	defer closeTrace(tw)

	agent := newAgentFor(agentOpts{
		Key: key, Model: *model, BaseURL: *baseURL, RunID: state.RunID,
		MaxSteps: maxStepsFor(fs, *maxSteps, state), Budget: *budget, Stream: *stream, Memory: *remember, Exec: *allowExec, Trust: trusted,
		ToolTimeout: *toolTimeout, HTTPTimeout: *httpTimeout,
		Allow: splitList(*allow), Approve: gated,
		Workspace: *workspace,
		MCPTools:  mcpTools,
		Store:     store, Trace: tw,
	})
	state.BaseURL = *baseURL
	state.Workspace = *workspace
	state.ContextBudget = *budget
	state.Memory = *remember
	fmt.Fprintf(os.Stderr, "run %s  model=%s\n", state.RunID, *model)

	return execute(ctx, agent, state, *stream)
}

// cmdResume continues an interrupted run from its checkpoint.
//
// Run takes a *State precisely so this is load-and-call rather than a second
// code path: the loop cannot tell a resumed run from a fresh one.
func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	baseURL := fs.String("base-url", "", "OpenAI-compatible endpoint (defaults to endpoint from checkpoint)")
	maxSteps := fs.Int("max-steps", defaultMaxSteps, maxStepsHelp)
	allow := fs.String("allow", "", "narrow the tools this run may call; it can never widen the grant in the checkpoint")
	workspace := fs.String("workspace", "", "directory the file tools are confined to (defaults to the checkpoint's)")
	mcpConfig := fs.String("mcp-config", envOr("ARIADNE_MCP_CONFIG", ""), "JSON file listing MCP servers to start")
	approve := fs.String("approve", "", "add tools needing approval; a gate in the checkpoint cannot be dropped here")
	trust := fs.String("trust", "", "MCP tools, or a gated built-in (web_fetch, edit_file, write_file), that run without approval; every other one asks first")
	allowExec := fs.Bool("exec", false, "offer the exec tool: runs a program in the workspace, and every call asks first")
	budget := fs.Int("context-budget", 0, "compact the conversation when the prompt exceeds this many tokens (0: never)")
	stream := fs.Bool("stream", false, "print tokens and tool calls as they arrive")
	remember := fs.Bool("remember", false, "let the run read and append to `MEMORY.md`")
	toolTimeout := fs.Duration("tool-timeout", defaultToolTimeout, "abandon a tool call that runs longer than this (0: never)")
	httpTimeout := fs.Duration("http-timeout", defaultHTTPTimeout, "bound one provider request, body included (0: only the context)")

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

	mem := *remember || state.Memory
	rememberFor := ""
	if mem {
		rememberFor = "validate"
	}

	if err := checkResumeGrants(mem, splitList(*allow), state); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitUsage
	}

	endpoint := resolveEndpoint(*baseURL, state)

	key, envName := apiKey(endpoint)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne resume: no api key for %s — run `ariadne setup`, or set %s\n", endpoint, envName)
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
	budgetVal := resolveBudget(*budget, state)
	workspaceDir := resolveWorkspace(*workspace, state)

	mcpTools, closeMCP, err := connectMCP(ctx, *mcpConfig, rememberFor, workspaceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitUsage
	}
	defer closeMCP()
	// After connecting, not before: -allow and -approve can name a tool an MCP
	// server provides, and checking against the local registry alone made an
	// MCP tool impossible to gate or grant.
	if err := checkNames(withRemote(newRegistry(rememberFor, workspaceDir, *allowExec).Defs(), mcpTools), splitList(*allow), splitList(*approve)); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitUsage
	}
	gated, err := gateMCP(splitList(*approve), splitList(*trust), mcpTools)
	trusted := splitList(*trust)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne resume: %v\n", err)
		return exitUsage
	}
	// Memory turned on for a run that started without it. The checkpoint's
	// system prompt wins on resume, so the notes have to be added to it here or
	// the tool would be present with nothing behind it. Recorded, so the next
	// resume knows it is already there rather than matching on a fence marker
	// that could be reworded.
	if mem && !state.Memory {
		state.System += memoryPrompt(true)
		state.Memory = true
	}
	agent := newAgentFor(agentOpts{
		Key: key, Model: state.Model, BaseURL: endpoint, RunID: state.RunID,
		MaxSteps: maxStepsFor(fs, *maxSteps, state), Budget: budgetVal, Stream: *stream, Memory: mem, Exec: *allowExec, Trust: trusted,
		ToolTimeout: *toolTimeout, HTTPTimeout: *httpTimeout,
		Allow: splitList(*allow), Approve: gated,
		Workspace: workspaceDir,
		MCPTools:  mcpTools,
		Store:     store, Trace: tw,
	})

	fmt.Fprintf(os.Stderr, "resume %s  model=%s  from step %d (%d messages)\n",
		state.RunID, state.Model, state.Steps, len(state.Messages))

	return execute(ctx, agent, state, *stream)
}

// execute runs the agent and reports. Shared by run and resume so the two
// cannot drift in how they print or what they exit with.
func execute(ctx context.Context, agent *loop.Agent, state *loop.State, streamed bool) int {
	started := time.Now()
	answer, err := agent.Run(ctx, state)
	elapsed := time.Since(started).Round(time.Millisecond)

	if err != nil {
		fmt.Fprintf(os.Stderr, "run %s failed after %d steps in %s: %v\n",
			state.RunID, state.Steps, elapsed, err)
		switch {
		case errors.Is(err, loop.ErrStepLimit):
			// The ceiling is per call, so a plain resume grants a fresh budget;
			// raising it is for a single turn that genuinely needs more room.
			fmt.Fprintf(os.Stderr, "hint: ariadne resume %s\n", state.RunID)
		case errors.Is(err, context.Canceled):
			fmt.Fprintf(os.Stderr, "cancelled; resume with: ariadne resume %s\n", state.RunID)
		}
		return exitFail
	}

	// The answer goes to stdout, which is the contract that makes
	// `ariadne run ... > answer.txt` useful. The one exception is a streamed
	// run whose stdout is a terminal: the answer has already scrolled past on
	// stderr, and printing it again is not a second copy, it is the same answer
	// twice. Redirected stdout still gets it, because the streamed copy went to
	// stderr and the file would otherwise be empty.
	if !streamed || !isTerminal(os.Stdout) {
		fmt.Println(answer)
	}
	noteUnopened(os.Stderr, state)
	fmt.Fprintf(os.Stderr, "run %s  steps=%d  cost=%s  %s\n",
		state.RunID, state.Steps, costText(state.Cost, state.UnpricedSteps > 0, 4), elapsed)
	return exitOK
}
