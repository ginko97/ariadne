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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/config"
	"github.com/ginko97/ariadne/internal/dotenv"
	"github.com/ginko97/ariadne/internal/mcp"
	"github.com/ginko97/ariadne/internal/trace"
)

const (
	// defaultModel belongs to Gemini's endpoint, and the pairing is the point:
	// the two flags default independently, so a bare id aimed at a gateway that
	// namespaces everything is a mismatch nobody asked for. OpenRouter resolved
	// "gemini-2.5-flash" to "google/gemini-2.5-flash" silently — visible only
	// because the served model is now recorded — and a stricter endpoint would
	// have refused it outright.
	defaultModel = "gemini-2.5-flash"
	// defaultOpenRouterModel is this project's pinned eval baseline, chosen by
	// measurement rather than from a price table: 6/6 on the task set at
	// $0.000031 a task, and the cheapest model that actually called the tool.
	defaultOpenRouterModel = "deepseek/deepseek-v4-flash-0731"
	defaultOpenAIModel     = "gpt-4o-mini"
	defaultXAIModel        = "grok-2"
	// defaultHuggingFaceModel had the most live tool-capable providers on
	// router.huggingface.co when it was added (10 of them, 2026-09-24), and
	// is among the cheapest there. Hugging Face's list moves; setup checks
	// the model before saving it.
	defaultHuggingFaceModel = "openai/gpt-oss-120b"
	// OpenRouter by default: one key reaches most models, and it is what
	// `ariadne setup` offers first. defaultModelFor pairs it with
	// defaultOpenRouterModel.
	defaultBaseURL = "https://openrouter.ai/api/v1"
	// Loopback only, and the port is the only part an operator can change:
	// a --port int cannot be spelled 0.0.0.0. Settled 2026-09-10.
	defaultPort = 7357
	historyDir  = "eval/history"
	// defaultToolTimeout bounds one tool call from the command line, where the
	// Agent's own default of 0 means unlimited. Same split as MaxSteps: a
	// library caller decides for itself, a job gets a limit whether or not
	// anybody remembered to ask for one.
	defaultToolTimeout = 60 * time.Second
	// One provider request. Generous because the cost of being wrong is a run
	// that cannot be resumed, not a run that is slow.
	defaultHTTPTimeout = 300 * time.Second
)

// Where this process keeps its data. The values here are what a checkout has
// always used and what the tests see; main replaces them with paths under the
// home directory internal/config resolves, so an installed binary has one
// history wherever it is run from.
var (
	runsDir          = "runs"
	defaultWorkspace = "workspace"
	// Outside the workspace on purpose: if the notes lived where the tools are
	// confined, write_file could rewrite them and every rule in
	// internal/memory would be decoration.
	memoryFile = "MEMORY.md"
)

// Exit codes: 0 the run succeeded, 1 it failed, 2 the command line was wrong.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// homeDir is where this process keeps its data (internal/config), printed at
// startup and shown in the page so nobody has to guess which runs/ it is.
var homeDir string

// checkoutHome is the ariadne checkout to use as the home directory, or ""
// for the user's data directory.
//
// Only a development build uses a checkout. A released binary started from a
// terminal inside the source tree used to take the checkout's runs/ and .env
// as well, and a day's conversations landed there instead of in
// %AppData%\ariadne, splitting the history in two (runs/run_20260923T202458_b47a5e).
// Where a released binary keeps its data must not depend on the folder it was
// started from; ARIADNE_HOME is the way to choose.
//
// version is main.version as the release build stamps it, not versionString:
// since Go 1.24 a plain `go build` in the checkout carries a VCS
// pseudo-version (v0.6.8-0.20260923184754-d4e4eef93430+dirty), and deciding
// on that sent every local build to the user directory as well.
func checkoutHome(version string, find func() (string, bool)) string {
	if version != "dev" {
		return ""
	}
	repo, _ := find()
	return repo
}

func main() {
	// Keys and settings, in precedence order: the process environment, a
	// checkout's .env (development), then config.env in the home directory
	// (an installed binary, written by `ariadne setup`). Each load leaves
	// variables already set alone, so the order of calls is the precedence.
	// Best effort: none of them has to exist.
	snapshotShellEnv()
	repo := checkoutHome(version, dotenv.Repo)
	if repo != "" {
		_ = dotenv.LoadFile(filepath.Join(repo, ".env"))
	}
	paths, err := config.Resolve(os.Getenv, repo, os.UserConfigDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne: %v\n", err)
		os.Exit(exitFail)
	}
	if len(os.Args) < 2 || os.Args[1] != "setup" {
		_ = dotenv.LoadFile(paths.Env)
	}
	runsDir, defaultWorkspace, memoryFile = paths.Runs, paths.Workspace, paths.Memory
	homeWorkspace = paths.Workspace
	// A default folder saved in the settings panel replaces the home
	// directory's workspace for every command; -workspace still wins for one
	// start.
	if folder, warning := savedWorkspace(os.Getenv); folder != "" {
		defaultWorkspace = folder
	} else if warning != "" {
		fmt.Fprintf(os.Stderr, "ariadne: %s\n", warning)
	}
	configEnvFile = paths.Env
	homeDir = paths.Home
	mcp.ClientVersion = versionString()

	if len(os.Args) < 2 {
		if startUIInstead(launchedFromExplorer()) {
			// Double-clicked. Usage printed here goes into a console that
			// Windows destroys as this process returns, so the whole download
			// ends in a window that blinks and vanishes. Start the thing the
			// README tells people to start instead.
			fmt.Fprintln(os.Stderr, "ariadne: no command given, so starting the browser interface.")
			fmt.Fprintln(os.Stderr, "Close this window to stop it. For the commands, run: ariadne --help")
			os.Exit(cmdUI(nil))
		}
		usage()
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "setup":
		os.Exit(cmdSetup(os.Args[2:]))
	case "version", "-version", "--version":
		fmt.Println("ariadne " + versionString())
		os.Exit(exitOK)
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "resume":
		os.Exit(cmdResume(os.Args[2:]))
	case "chat":
		os.Exit(cmdChat(os.Args[2:]))
	case "ui":
		os.Exit(cmdUI(os.Args[2:]))
	case "eval":
		os.Exit(cmdEval(os.Args[2:]))
	case "traces":
		os.Exit(cmdTraces(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "ariadne: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
}

func usage() { fmt.Fprint(os.Stderr, usageText) }

// usageText is checked against every flag the commands register by
// TestUsageNamesEveryFlag. It is hand-written, and it drifted: -mcp-config
// shipped in 0ec4602 with no line here, the same way the README once omitted
// chat and ui entirely.
const usageText = `ariadne — a personal AI assistant that asks before it acts

usage:
  ariadne setup  [flags]                 choose a provider and store its key (start here)
  ariadne run    [flags] <task>
  ariadne resume [flags] <run-id>
  ariadne chat   [flags] [run-id]        talk; with no id, the first line typed is the task
  ariadne ui     [flags]                 talk in the browser; with no key, set one up there
  ariadne eval   [flags]                 score a task set, one row per model
  ariadne traces [flags] [text]          search the JSONL traces every run writes
  ariadne version                        print the version

flags:
  -model          model id                   (env ARIADNE_MODEL)
  -base-url       OpenAI-compatible endpoint (env ARIADNE_BASE_URL)
  -max-steps      ceiling on loop iterations per turn (default 10; 25 for a brief)
  -allow          comma-separated tools this run may call (default: all)
  -workspace      directory the file tools are confined to (default workspace)
  -approve        tools needing a yes before each call (terminal; the browser under ui)
  -context-budget compact the conversation past this many prompt tokens (0: never)
  -stream         print tokens and tool calls as they arrive
  -remember       let the run read and append to MEMORY.md (off by default)
  -exec           offer exec: run a program (argv, no shell) in the workspace;
                  every call asks, and no flag exempts it (off by default)
  -tool-timeout   abandon a tool call that runs longer than this (default 1m0s)
  -http-timeout   bound one provider request, body included (default 5m0s)
  -mcp-config     JSON file listing MCP servers to start (env ARIADNE_MCP_CONFIG)
                  each server's tools are named <server>__<tool>, and every one
                  asks for approval unless named in -trust
  -trust          MCP tools, or a gated built-in, that run without approval,
                  e.g. fs__read_text_file. web_fetch, edit_file and write_file
                  ask every time unless named here; exec always asks
  -task           markdown file containing the task (shows brief before running)

chat flags: same as run/resume. While chatting, ` + "`/help`" + ` lists the commands.

ui flags: the same, without -stream and -remember, plus
  -port           loopback port to listen on (default 7357, 0: any free; env ARIADNE_PORT)
  -no-open        do not open the browser

eval flags:
  -models         comma-separated model ids  (default: ARIADNE_MODEL)
  -tasks          path to task set           (default testdata/tasks.json)
  -min-pass-rate  exit non-zero if any model scores below this
  -repeat         run each task this many times; it passes only if all do (default 1)
  -save           write scorecard to eval/history and report regressions

traces flags (must come before the search text):
  -run            limit to runs whose id contains this
  -kind           comma-separated event kinds
  -tool           comma-separated tool names
  -errors         only events recording something going wrong
  -stats          aggregate instead of listing
  -limit          maximum events to print (default 50, 0: all)

environment:
  ARIADNE_API_KEY   key for any endpoint; without it, the provider's own by host:
                    OPENROUTER_API_KEY, GEMINI_API_KEY, OPENAI_API_KEY, XAI_API_KEY,
                    HF_TOKEN (Hugging Face).
                    Other endpoints (Groq, Ollama, ...) need ARIADNE_API_KEY
  ARIADNE_HOME      where runs, workspace, MEMORY.md and config.env live
                    (default: your user config directory, e.g. %AppData%\ariadne)
  ARIADNE_WORKSPACE default folder for new conversations (set in the page's
                    settings; -workspace wins for one start)

Keys come from the environment, then, for a development build run inside an
ariadne source checkout, that checkout's .env, then config.env (written by
ariadne setup). A released binary never uses a checkout's .env or runs/.

setup flags:
  -provider       openrouter (default), openai, gemini, xai, huggingface, ollama, or other
  -base-url       endpoint, for -provider other (or Ollama somewhere other than
                  http://localhost:11434/v1)
  -model          model to use by default
  -no-check       write the config without testing the key
`

// envInt reads an int from the environment, falling back when unset or
// unparseable — a malformed ARIADNE_PORT should not stop the server starting.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
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
// otherwise the host decides, and only a host this function recognises gets a
// provider's key.
//
// Deciding by host matters as soon as .env holds more than one provider's key:
// a fixed precedence order would send the Gemini key to OpenRouter and produce
// a 401 that reads as "bad key" rather than "wrong key".
//
// An unrecognised host gets ARIADNE_API_KEY or nothing. It used to fall back to
// whichever provider key was set, which sent an OpenRouter or OpenAI key to any
// endpoint anybody typed into -base-url, or recorded on a checkpoint. That is
// the leak 6b4845c closed for a known host with an unset key, from the other
// side.
//
// A loopback endpoint with no key set gets localNoKey: Ollama and other local
// servers ask for none, and every command refuses to start without one. The
// placeholder is sent only to this machine, and ARIADNE_API_KEY still wins for
// a local server that does want a key. Storing a placeholder ARIADNE_API_KEY
// in config.env instead would have outranked a real OPENROUTER_API_KEY the
// moment someone pointed -base-url back at OpenRouter.
//
// Matched on the parsed host, by exact name or subdomain. A substring match
// gave max.ai the xAI key, because "max.ai" contains "x.ai".
func apiKey(baseURL string) (key, envName string) {
	if v := os.Getenv("ARIADNE_API_KEY"); v != "" {
		return v, "ARIADNE_API_KEY"
	}
	if name := providerKeyName(baseURL); name != "" {
		return os.Getenv(name), name
	}
	if isLoopbackURL(baseURL) {
		return localNoKey, ""
	}
	return "", "ARIADNE_API_KEY"
}

// localNoKey is the key a loopback endpoint gets when none is set.
const localNoKey = "no-key"

// isLoopbackURL says baseURL's host is this machine: localhost or a loopback
// address. Not the local network — a server on another machine is somebody
// else's, as far as keys go.
func isLoopbackURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		if u2, err2 := url.Parse("//" + baseURL); err2 == nil {
			u = u2
		} else {
			return false
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// providerKeys maps a provider's domain to the variable holding its key.
var providerKeys = []struct{ domain, env string }{
	{"openrouter.ai", "OPENROUTER_API_KEY"},
	{"googleapis.com", "GEMINI_API_KEY"},
	{"x.ai", "XAI_API_KEY"},
	{"openai.com", "OPENAI_API_KEY"},
	// HF_TOKEN, the name Hugging Face's own tools read, so a token already
	// exported for them is found; it goes only to huggingface.co hosts.
	{"huggingface.co", "HF_TOKEN"},
}

// providerKeyName returns the key variable for baseURL's host, or "" for a host
// that is not a known provider's domain or a subdomain of one.
func providerKeyName(baseURL string) string {
	u, err := url.Parse(baseURL)
	host := ""
	if err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" {
		if u2, err2 := url.Parse("//" + baseURL); err2 == nil {
			host = strings.ToLower(u2.Hostname())
		}
	}
	for _, p := range providerKeys {
		if host == p.domain || strings.HasSuffix(host, "."+p.domain) {
			return p.env
		}
	}
	return ""
}

// closeTrace reports a broken trace without failing anything: a run that
// finished is still a run, but a trace that silently stopped recording would
// otherwise look like a run that never did those things.
func closeTrace(tw *trace.Writer) {
	if err := tw.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: trace incomplete: %v\n", err)
	}
}
