package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ginko97/ariadne/internal/cite"
	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/memory"
	"github.com/ginko97/ariadne/internal/trace"
)

func cmdChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	model := fs.String("model", envOr("ARIADNE_MODEL", ""), "model id (default depends on -base-url)")
	baseURL := fs.String("base-url", "", "OpenAI-compatible endpoint (fresh: default "+defaultBaseURL+"; resumed: checkpoint's unless overridden)")
	maxSteps := fs.Int("max-steps", defaultMaxSteps, maxStepsHelp)
	allow := fs.String("allow", "", "comma-separated tools this run may call (resume can only narrow it)")
	workspace := fs.String("workspace", "", "directory the file tools are confined to (fresh: default workspace; resumed: checkpoint's unless overridden)")
	mcpConfig := fs.String("mcp-config", envOr("ARIADNE_MCP_CONFIG", ""), "JSON file listing MCP servers to start")
	approve := fs.String("approve", "", "tools needing a yes on the terminal before each call (resume can only add)")
	trust := fs.String("trust", "", "MCP tools, or a gated built-in (web_fetch, edit_file, write_file), that run without approval; every other one asks first")
	allowExec := fs.Bool("exec", false, "offer the exec tool: runs a program in the workspace, and every call asks first")
	budget := fs.Int("context-budget", 0, "compact the conversation past this many prompt tokens (0: never)")
	stream := fs.Bool("stream", false, "print tokens and tool calls as they arrive")
	remember := fs.Bool("remember", false, "let the run read and append to `MEMORY.md`")
	toolTimeout := fs.Duration("tool-timeout", defaultToolTimeout, "abandon a tool call that runs longer than this (0: never)")
	httpTimeout := fs.Duration("http-timeout", defaultHTTPTimeout, "bound one provider request, body included (0: only the context)")
	taskFile := fs.String("task", "", "markdown file containing the task (shows brief before running)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if len(fs.Args()) > 1 {
		fmt.Fprintf(os.Stderr, "ariadne chat: unexpected arguments %v (flags must come before the run id)\n", fs.Args()[1:])
		return exitUsage
	}

	resuming := len(fs.Args()) == 1
	if resuming && *taskFile != "" {
		fmt.Fprintln(os.Stderr, "ariadne chat: cannot use -task when resuming an existing conversation")
		return exitUsage
	}

	var briefContent string
	if !resuming && *taskFile != "" {
		t, code := readTaskFile("ariadne chat", *taskFile)
		if code != exitOK {
			return code
		}
		briefContent = t
		fmt.Fprintf(os.Stderr, "task from %s:\n%s\n\n", *taskFile, briefContent)
	}

	store := &loop.Store{Dir: runsDir}

	var state *loop.State
	if resuming {
		s, err := store.Load(fs.Args()[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
			return exitFail
		}
		state = s
	}

	mem := *remember
	if resuming {
		mem = mem || state.Memory
	}
	rememberFor := ""
	if mem {
		rememberFor = "validate"
	}
	if resuming {
		if err := checkResumeGrants(mem, splitList(*allow), state); err != nil {
			fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
			return exitUsage
		}
	} else if *remember && len(splitList(*allow)) > 0 && !contains(splitList(*allow), "remember") {
		fmt.Fprintln(os.Stderr, "ariadne chat: -remember with -allow needs remember in the list")
		return exitUsage
	}

	endpoint := *baseURL
	if resuming {
		endpoint = resolveEndpoint(*baseURL, state)
	} else if endpoint == "" {
		endpoint = envOr("ARIADNE_BASE_URL", defaultBaseURL)
	}

	// After the endpoint is known, because that is what the default depends on.
	// A resumed conversation already recorded its own model, so this only
	// decides what a fresh one starts with.
	if *model == "" {
		*model = defaultModelFor(endpoint)
	}

	// A resumed conversation keeps the directory it was reading, unless the
	// flag says otherwise. A fresh one takes the flag, which defaults to
	// "workspace" — so the common case is unchanged.
	workspaceDir := *workspace
	if resuming {
		workspaceDir = resolveWorkspace(*workspace, state)
	} else if workspaceDir == "" {
		workspaceDir = defaultWorkspace
	}

	// Started once for the session rather than per turn: a subprocess per
	// message would pay the handshake every time and lose whatever state the
	// server keeps between calls.
	mcpTools, closeMCP, err := connectMCP(context.Background(), *mcpConfig, rememberFor, workspaceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
		return exitUsage
	}
	defer closeMCP()
	// After connecting, not before: -allow and -approve can name a tool an MCP
	// server provides, and checking against the local registry alone made an
	// MCP tool impossible to gate or grant.
	if err := checkNames(withRemote(newRegistry(rememberFor, workspaceDir, *allowExec).Defs(), mcpTools), splitList(*allow), splitList(*approve)); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
		return exitUsage
	}
	gated, err := gateMCP(splitList(*approve), splitList(*trust), mcpTools)
	trusted := splitList(*trust)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
		return exitUsage
	}

	key, envName := apiKey(endpoint)
	if key == "" {
		fmt.Fprintf(os.Stderr, "ariadne chat: no api key for %s — run `ariadne setup`, or set %s\n", endpoint, envName)
		return exitUsage
	}

	modelExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "model" {
			modelExplicit = true
		}
	})

	runID := newRunID()
	startModel := *model
	if resuming {
		if modelExplicit && state.HasPendingToolCalls() {
			fmt.Fprintln(os.Stderr, "ariadne chat: cannot switch model while tool calls are pending")
			return exitUsage
		}
		runID = state.RunID
		startModel = resolveModel(*model, modelExplicit, state)
	}

	tw, err := trace.NewFileWriter(runsDir, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne chat: %v\n", err)
		return exitFail
	}
	defer closeTrace(tw)

	budgetVal := *budget
	if resuming {
		budgetVal = resolveBudget(*budget, state)
		// Memory turned on for a run that started without it — same
		// reconciliation cmdResume does, and for the same reason: the system
		// prompt in the checkpoint wins on resume, so the notes have to be
		// added to it here or the tool would be offered with nothing behind it.
		if mem && !state.Memory {
			state.System += memoryPrompt(true)
			state.Memory = true
		}
	}

	// The same reader the approval prompts use: one owner of os.Stdin for the
	// process, so a prompt that times out cannot leave a goroutine behind to
	// swallow the next message typed here.
	stdinReader := stdinSource()
	// Built on first use: a chat that never asks for the list never fetches it.
	var models *llm.ModelCache

	agent := newAgentFor(agentOpts{
		Key: key, Model: startModel, BaseURL: endpoint, RunID: runID,
		MaxSteps: maxStepsFor(fs, *maxSteps, state), Budget: budgetVal, Stream: *stream, Memory: mem, Exec: *allowExec, Trust: trusted,
		ToolTimeout: *toolTimeout, HTTPTimeout: *httpTimeout,
		Allow: splitList(*allow), Approve: gated,
		ApproveFn: approveOnTerminalReader(os.Stdin, stdinReader, workspaceDir),
		Workspace: workspaceDir,
		MCPTools:  mcpTools,
		Store:     store, Trace: tw,
	})

	if resuming {
		fmt.Fprintf(os.Stderr, "chat %s  model=%s  from step %d (%d messages)  (/help for commands, /exit to leave)\n",
			state.RunID, state.Model, state.Steps, len(state.Messages))
	} else if briefContent != "" {
		state = loop.NewState(runID, briefContent)
		state.Brief = *taskFile
		state.Workspace = workspaceDir
		state.Memory = mem
		agent.MaxSteps = maxStepsFor(fs, *maxSteps, state)
		// The same line a typed first message prints: without it a brief
		// conversation has no id on screen to resume it by.
		fmt.Fprintf(os.Stderr, "chat %s  model=%s\n", state.RunID, agent.Model)
		if err := chatTurn(agent, state, *stream); err != nil {
			fmt.Fprintf(os.Stderr, "! %v\n", err)
		}
	} else {
		fmt.Fprint(os.Stderr, "chat: type your message (/help for commands, /exit to leave)\n")
	}

	for {
		if state != nil && state.HasPendingToolCalls() {
			fmt.Fprintln(os.Stderr, "finishing an interrupted turn...")
			if err := chatTurn(agent, state, *stream); err != nil {
				fmt.Fprintf(os.Stderr, "! %v\n", err)
			}
			continue
		}

		fmt.Fprint(os.Stderr, "> ")
		line, err := stdinReader.next(context.Background(), 0)
		if err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)) {
			break // Ctrl-D or error
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if err != nil {
				break
			}
			continue
		}
		// Exact match, or the name separated by a space. A bare prefix cut
		// makes "/modeled" a switch to the model "ed": the command silently
		// reconfigures the conversation, and the 404 lands a turn later
		// naming the provider rather than the typo.
		if rest, ok := strings.CutPrefix(line, "/models"); ok &&
			(rest == "" || strings.HasPrefix(rest, " ")) {
			if models == nil {
				models = llm.NewModelCache(*model)
				unsupported, local := modelListFor(endpoint)
				models.Reconfigure(*model, unsupported, local)
			}
			listModels(models, strings.TrimSpace(rest))
			continue
		}
		if line == "/help" || line == "/?" {
			fmt.Fprint(os.Stderr, chatCommands)
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		// Ctrl-D is not end-of-input on Windows. The console does not translate
		// it, so it arrives as a literal EOT inside the line — which TrimSpace
		// leaves alone, being a control character rather than whitespace — and
		// the line goes to the provider, which is asked to answer a keystroke.
		// Nobody types EOT at a chat prompt on purpose, so it is taken to mean
		// what it means everywhere else.
		if strings.TrimFunc(line, func(r rune) bool { return r == '\x04' }) == "" {
			break
		}
		if line == "/model" {
			current := agent.Model
			if state != nil && state.Model != "" {
				current = state.Model
			}
			fmt.Fprintf(os.Stderr, "current model: %s (usage: /model <model-name>)\n", current)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "/model "); ok {
			// line is already trimmed, so the prefix guarantees a name here.
			newModel := strings.TrimSpace(rest)
			// State first: a refusal must leave both unchanged, or the agent
			// switches, the state does not, and ChatTurn's sync fails the same
			// way on every turn after this one.
			if state != nil {
				if err := state.SetModel(newModel); err != nil {
					fmt.Fprintf(os.Stderr, "! %v\n", err)
					continue
				}
			}
			agent.Model = newModel
			fmt.Fprintf(os.Stderr, "model set to %s, takes effect next turn\n", agent.Model)
			continue
		}

		if memoryCommand(os.Stderr, memory.Store{Path: memoryFile}, mem, line) {
			continue
		}

		// A line meant as a command must not become a prompt. Sending /model-list
		// to the provider spends a request to be told the model has no tool for
		// listing models, which is both true and useless — and the typo is
		// invisible, because the reply reads like an ordinary refusal.
		//
		// Escaped with a leading double slash, so a question that genuinely
		// starts with one — about /etc/hosts, or /api/chat — is still askable.
		if strings.HasPrefix(line, "//") {
			line = line[1:]
		} else if strings.HasPrefix(line, "/") {
			fmt.Fprintf(os.Stderr, "unknown command %q\n%s", strings.Fields(line)[0], chatCommands)
			continue
		}

		if state == nil {
			state = loop.NewState(runID, line)
			state.BaseURL = endpoint
			state.ContextBudget = *budget
			state.Memory = mem
			state.Workspace = workspaceDir
			fmt.Fprintf(os.Stderr, "chat %s  model=%s\n", state.RunID, agent.Model)
			if err := chatTurn(agent, state, *stream); err != nil {
				fmt.Fprintf(os.Stderr, "! %v\n", err)
			}
			continue
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		answer, err := agent.ChatTurn(ctx, state, line)
		stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "! %v\n", err)
			continue
		}
		printAnswer(answer, *stream)
		noteUnopened(os.Stderr, state)
	}
	return exitOK
}

// chatCommands is what the REPL answers to, printed on /help and on a typo.
// One constant, so the two places that show it cannot disagree.
const chatCommands = `commands:
  /models [text]  list tool-capable models, filtered by substring
  /models all     list every one of them
  /model <id>     switch model starting next turn
  /model          show the current model
  /memory         list remembered facts, numbered
  /remember <fact> save a fact exactly as typed (needs -remember)
  /forget <n>     delete fact n
  /help           this list
  //text          send a line that really does start with a slash
  /exit           leave (Ctrl-D on Unix; Ctrl-Z then Enter on Windows)
`

// noModelList explains why there is no catalogue, and how to get one.
//
// One function because the REPL and the browser hit the identical condition,
// and they had drifted: the browser named the remedy and the REPL named only
// the problem. A message that says what is wrong without saying what to do
// leaves somebody staring at "1 of 1" wondering what they broke — which is
// exactly what happened.
func noModelList(endpoint string) string {
	return fmt.Sprintf(
		"%s publishes no model list this can read, so only the configured model "+
			"is offered. For the full catalogue start with "+
			"-base-url https://openrouter.ai/api/v1", endpoint)
}

// listModels prints the catalogue the picker in the browser already has.
//
// Filtered rather than paged. The gateway offers several hundred tool-capable
// models and a terminal that prints them all has told you nothing; a substring
// is how anybody would look for one anyway. Without a filter it prints a count
// and asks for one, rather than scrolling the answer off the screen.
//
// Price is shown and the list is not ordered by it. The cheapest tool-capable
// model on this gateway scored 3/6 in this project's own evals because it
// answers from its weights instead of calling the tool, so ordering by price
// would recommend the model the repo already measured as the wrong pick.
func listModels(cache *llm.ModelCache, filter string) {
	rows, source, warning := cache.Get(context.Background())
	if warning != "" {
		fmt.Fprintf(os.Stderr, "! %s\n", warning)
	}

	// "all" asks for the whole catalogue on purpose. Without it, the only way
	// past the guard below was a filter that happens to match everything —
	// "/models /" works here because every id on this gateway is namespaced,
	// and would not on one whose ids are bare. A trick that depends on the
	// shape of the data is not an interface.
	all := strings.EqualFold(filter, "all")
	if all {
		filter = ""
	}

	var shown []llm.ModelRow
	for _, r := range rows {
		if filter == "" || strings.Contains(strings.ToLower(r.ID), strings.ToLower(filter)) {
			shown = append(shown, r)
		}
	}

	if filter == "" && !all && len(rows) > 20 {
		fmt.Fprintf(os.Stderr, "%d models (%s). Narrow it: /models claude, /models gpt — or /models all\n",
			len(rows), source)
		return
	}
	if len(shown) == 0 {
		fmt.Fprintf(os.Stderr, "no model id contains %q (%d available)\n", filter, len(rows))
		return
	}
	for _, r := range shown {
		// A model on this computer has no price to show, not an unknown one.
		if source == "local" {
			fmt.Fprintf(os.Stderr, "  %s\n", r.ID)
			continue
		}
		price := "     —"
		if r.PromptPerMTok > 0 {
			price = fmt.Sprintf("%6.2f", r.PromptPerMTok)
		}
		fmt.Fprintf(os.Stderr, "  %-44s %s /Mtok\n", r.ID, price)
	}
	fmt.Fprintf(os.Stderr, "%d of %d (%s) — switch with /model <id>\n", len(shown), len(rows), source)
}

// chatTurn runs the agent with no new message — the first turn of a fresh
// chat, where NewState already carries the task, and flushing a batch a
// cancelled turn left pending. Its own context, scoped to this call only, so
// Ctrl-C here cancels this turn and nothing waiting after it.
func chatTurn(agent *loop.Agent, state *loop.State, streamed bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	answer, err := agent.Run(ctx, state)
	if err != nil {
		return err
	}
	printAnswer(answer, streamed)
	noteUnopened(os.Stderr, state)
	return nil
}

// printAnswer applies the same stdout contract execute does: redirected
// output always gets the answer, a streamed terminal does not get it twice.
func printAnswer(answer string, streamed bool) {
	if !streamed || !isTerminal(os.Stdout) {
		fmt.Println(answer)
	}
}

// noteUnopened tells the operator which URLs the turn just finished cited
// that no call in the conversation opened (internal/cite). Stderr, like every
// other note: stdout is the answer and nothing else.
func noteUnopened(w io.Writer, state *loop.State) {
	urls := cite.Unopened(state.Messages)
	if len(urls) == 0 {
		return
	}
	fmt.Fprintf(w, "cited but never opened in this conversation (%d):\n", len(urls))
	for _, u := range urls {
		fmt.Fprintf(w, "  %s\n", u)
	}
}
