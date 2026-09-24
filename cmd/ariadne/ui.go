package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/memory"
	"github.com/ginko97/ariadne/internal/server"
	"github.com/ginko97/ariadne/internal/trace"
)

// cmdUI serves the chat endpoint on loopback.
//
// Approvals are requested interactively in the browser over SSE and answered
// via POST /api/approve. Memory is enabled with forced approval gating: every
// fact Ariadne remembers must be explicitly approved in the browser before being
// written to MEMORY.md, and past facts are curated in the Memory drawer.
func cmdUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	port := fs.Int("port", envInt("ARIADNE_PORT", defaultPort), "loopback port to listen on (0: pick a free one)")
	noOpen := fs.Bool("no-open", false, "do not open the browser")
	model := fs.String("model", envOr("ARIADNE_MODEL", ""), "model id (default depends on -base-url)")
	baseURL := fs.String("base-url", envOr("ARIADNE_BASE_URL", defaultBaseURL), "OpenAI-compatible endpoint")
	maxSteps := fs.Int("max-steps", defaultMaxSteps, maxStepsHelp)
	allow := fs.String("allow", "", "comma-separated tools a conversation may call (default: all)")
	approve := fs.String("approve", "", "tools needing approval in the browser before each call")
	trust := fs.String("trust", "", "MCP tools, or a gated built-in (web_fetch, edit_file, write_file), that run without approval; every other one asks first")
	allowExec := fs.Bool("exec", false, "offer the exec tool: runs a program in the workspace, and every call asks first")
	workspace := fs.String("workspace", "", "default folder for new conversations; the page can choose another, and an existing conversation always keeps its own")
	mcpConfig := fs.String("mcp-config", envOr("ARIADNE_MCP_CONFIG", ""), "JSON file listing MCP servers to start")
	budget := fs.Int("context-budget", 0, "compact the conversation past this many prompt tokens (0: never)")
	toolTimeout := fs.Duration("tool-timeout", defaultToolTimeout, "abandon a tool call that runs longer than this (0: never)")
	httpTimeout := fs.Duration("http-timeout", defaultHTTPTimeout, "bound one provider request, body included (0: only the context)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *model == "" {
		*model = defaultModelFor(*baseURL)
	}

	workspaceDir := *workspace
	if workspaceDir == "" {
		workspaceDir = defaultWorkspace
	}

	// For the life of the server, not per request: a subprocess started and
	// stopped around every turn would pay its handshake each time, and the
	// agent factory has no moment to close one.
	mcpTools, closeMCP, err := connectMCP(context.Background(), *mcpConfig, "", workspaceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne ui: %v\n", err)
		return exitUsage
	}
	defer closeMCP()
	// After connecting, not before: -allow and -approve can name a tool an MCP
	// server provides, and checking against the local registry alone made an
	// MCP tool impossible to gate or grant.
	if err := checkNames(withRemote(newRegistry("", workspaceDir, *allowExec).Defs(), mcpTools), splitList(*allow), splitList(*approve)); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne ui: %v\n", err)
		return exitUsage
	}
	gated, err := gateMCP(splitList(*approve), splitList(*trust), mcpTools)
	trusted := splitList(*trust)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne ui: %v\n", err)
		return exitUsage
	}

	// No key is not fatal here, unlike the other commands: the page opens on
	// its setup card, and conversations wait until a provider is chosen.
	key, envName := apiKey(*baseURL)
	if key == "" {
		fmt.Fprintf(os.Stderr, "no api key for %s (%s): set one up in the page that opens\n", *baseURL, envName)
	}
	prov := &uiProvider{baseURL: *baseURL, key: key, model: *model}

	store := &loop.Store{Dir: runsDir}

	// The folder a new conversation gets when the page sends none. Absolute,
	// because the page shows it and a relative path says nothing about where.
	serverWorkspace := *workspace
	if serverWorkspace == "" {
		serverWorkspace = defaultWorkspace
	}
	if abs, err := filepath.Abs(serverWorkspace); err == nil {
		serverWorkspace = abs
	}

	// The options every agent here is built from. One function for the agents
	// that run turns and for the probe the page's tool list is read from, so
	// the list cannot describe a different agent from the one that runs.
	// Declared before optsFor so it can read the server's current default
	// folder, which the settings panel can change while this runs.
	var srv *server.Server
	optsFor := func(runID string, state *loop.State, onDelta func(llm.Chunk), tw *trace.Writer) agentOpts {
		workspaceDir := conversationWorkspace(srv.DefaultFolder(), state)
		dropStoredMemory(state)

		budgetVal := *budget
		if state != nil {
			budgetVal = resolveBudget(*budget, state)
		}

		// Read once per agent, so a setup saved mid-turn applies from the next
		// turn rather than to half of this one.
		curURL, curKey, curModel := prov.current()
		endpoint, endpointKey := conversationEndpoint(curURL, curKey, state)

		return agentOpts{
			Key: endpointKey, Model: curModel, BaseURL: endpoint, RunID: runID,
			MaxSteps: maxStepsFor(fs, *maxSteps, state), Budget: budgetVal, Exec: *allowExec, Trust: trusted,
			ToolTimeout: *toolTimeout, HTTPTimeout: *httpTimeout,
			Allow:     splitList(*allow),
			Workspace: workspaceDir,
			MCPTools:  mcpTools,
			OnDelta:   onDelta,
			Approve:   gated,
			Memory:    true,
			// No ApproveFn: internal/server replaces Agent.Approve per request
			// with one that asks over the stream that request is holding, which
			// is a writer only the handler has. A denier here would be silently
			// overwritten, and leaving one would imply a fallback that does not
			// exist.
			Store: store, Trace: tw,
		}
	}

	// One agent per request. The trace writer is opened here and closed by the
	// returned cleanup, because a server has no end-of-main to defer to.
	newAgent := func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
		tw, err := trace.NewFileWriter(runsDir, runID)
		if err != nil {
			// A run that cannot be traced is still a run; say so and continue,
			// the same trade closeTrace makes at the other end.
			fmt.Fprintf(os.Stderr, "warning: trace unavailable for %s: %v\n", runID, err)
			tw = nil
		}
		agent := newAgentFor(optsFor(runID, state, onDelta, tw))
		if tw == nil {
			return agent, nil
		}
		return agent, func() { closeTrace(tw) }
	}

	srv = server.New(store, newAgent, newRunID)
	srv.MemoryStore = memory.Store{Path: memoryFile}
	srv.Version = versionString()
	srv.Home = homeDir
	srv.Tools = probeTools(func(runID string) agentOpts { return optsFor(runID, nil, nil, nil) })
	// The picker's fallback is the model this process was started with: the one
	// model known to work, because every turn here already uses it.
	//
	// The list itself only exists for OpenRouter. Pricing and the
	// tool-capability filter are extensions of that gateway, and its model ids
	// are namespaced for it — so offering that catalogue while pointed at
	// api.openai.com would fill the picker with ids the endpoint rejects. The
	// answer is one model and a reason, not a longer list of wrong ones.
	srv.Models = llm.NewModelCache(*model)
	srv.DefaultWorkspace = serverWorkspace
	flagFolder := ""
	if *workspace != "" {
		flagFolder = serverWorkspace
	}
	srv.SaveDefaultFolder = folderSaver(flagFolder)
	srv.PickFolder = folderPicker(runtime.GOOS, exec.LookPath)
	unsupported, local := modelListFor(*baseURL)
	srv.Models.Reconfigure(*model, unsupported, local)
	prov.models = srv.Models
	srv.Setup = &server.Setup{
		Status:    prov.status,
		Configure: prov.configure,
		OllamaModels: func(ctx context.Context, base string) ([]string, error) {
			if base == "" {
				base = ollamaURL
			}
			return ollamaModels(ctx, base)
		},
	}

	// Host is a constant, not a flag: the loopback guarantee is structural
	// rather than something an operator can mistype into 0.0.0.0.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil && *port != 0 {
		// Taken ports are common and fatal for no good reason. Fall back and
		// print where it actually landed rather than announcing a URL that was
		// never bound.
		fmt.Fprintf(os.Stderr, "port %d unavailable (%v), picking a free one\n", *port, err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne ui: %v\n", err)
		return exitFail
	}
	defer ln.Close()

	pageURL := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "ariadne ui  %s  model=%s\n", pageURL, *model)
	fmt.Fprintf(os.Stderr, "home: %s\n", homeDir)
	fmt.Fprintf(os.Stderr, "csrf token: %s\n", srv.CSRFToken)
	fmt.Fprintln(os.Stderr, "Ctrl-C to stop")

	// After the listener is bound, so the page is there when the browser
	// asks; requests that arrive before Serve wait in the accept backlog.
	if !*noOpen {
		if err := openBrowser(pageURL); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser (%v); open the URL above\n", err)
		}
	}

	if err := http.Serve(ln, srv.Routes()); err != nil {
		fmt.Fprintf(os.Stderr, "ariadne ui: %v\n", err)
		return exitFail
	}
	return exitOK
}
