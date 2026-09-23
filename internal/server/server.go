// Package server exposes a run over HTTP, for the browser half of v0.2.0.
//
// It adds no new notion of a conversation. A chat is a run, the same object
// cmd/ariadne already checkpoints, resumes, compacts and traces — this package
// only carries one over the wire. What it does add is everything the REPL
// never had to think about, because a REPL is one conversation on one
// goroutine and a server is neither: turns arriving concurrently for the same
// run, a writer that is only valid for the life of one request, and a caller
// who can disappear mid-answer.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/memory"
)

// AgentFactory builds the agent for one request.
//
// Per request rather than per server, because onDelta writes into that
// request's ResponseWriter and is valid exactly as long as the handler is.
// An Agent is a plain struct, so this costs a few allocations and buys the
// guarantee that no two turns ever share a delta sink.
//
// The second return closes whatever the agent opened — a trace file, in the
// only implementation that matters. A single-shot command can defer that to
// the end of main; a server cannot, because "the end" is when the process
// stops and the handles accumulate one per turn until then. May be nil.
type AgentFactory func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func())

// Server holds what outlives a request: the checkpoint store, how to build an
// agent, and which runs are busy.
//
// Deliberately not an Agent. One agent shared across requests would mean one
// delta sink shared across requests, which is the same conversation streamed
// into somebody else's browser.
type Server struct {
	Store       *loop.Store
	MemoryStore memory.Store
	NewAgent    AgentFactory

	// NewRunID mints the id for a fresh conversation. Injected rather than
	// implemented here: cmd/ariadne already has one, and a run id format
	// living in two packages is a format that will eventually differ in one.
	NewRunID func() string

	// Models is the picker's source. Nil disables the endpoint rather than
	// failing it, so a server can run without ever reaching the network.
	Models *llm.ModelCache

	// CSRFToken gates every mutating request. Loopback binding is not
	// protection on its own: any page the browser has open can POST to
	// localhost, and the worst case here is not noise, it is a page spending
	// somebody's API budget and reading the answer back. Settled 2026-09-10
	// for /setup; it applies to every endpoint that changes something.
	CSRFToken string

	// approvals carries a decision from POST /api/approve to the turn that is
	// blocked waiting for it. A turn cannot read its own answer: it is holding
	// the SSE stream the question went out on.
	approvals *approvals
	// approvalTimeout is how long a card waits; defaultApprovalTimeout unless
	// a test shortens it before the server starts serving.
	approvalTimeout time.Duration

	// Version is the build, for the settings panel and bug reports.
	Version string
	// Tools is what every conversation here is offered, and which of them
	// ask first, as the agent factory builds it. See handleAbout.
	Tools []ToolInfo

	// DefaultWorkspace is the folder a new conversation gets when the page
	// sends none — the server's -workspace, or the home directory's
	// workspace. Only a default: a conversation's recorded folder always
	// wins over it, and the page cannot change an existing one.
	DefaultWorkspace string

	// PickFolder opens the operating system's folder dialog and returns the
	// chosen path, or "" if it was cancelled. Nil means this machine has no
	// dialog, and the page offers typing a path instead.
	PickFolder func(ctx context.Context) (string, error)

	// Setup reads and changes the provider from the page. Nil when the
	// process was started with a provider and cannot change it. See setup.go.
	Setup *Setup

	// picking keeps a second dialog from stacking behind an open one.
	picking sync.Mutex

	mu   sync.Mutex
	live map[string]bool // run ids with a turn in flight
}

func New(store *loop.Store, newAgent AgentFactory, newRunID func() string) *Server {
	return &Server{
		Store:     store,
		NewAgent:  newAgent,
		NewRunID:  newRunID,
		CSRFToken: newToken(),
		approvals: newApprovals(),
		live:      map[string]bool{},

		approvalTimeout: defaultApprovalTimeout,
	}
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/memory", s.handleMemoryList)
	mux.HandleFunc("DELETE /api/memory", s.handleMemoryDelete)
	mux.HandleFunc("GET /api/runs/{id}", s.handleTranscript)
	mux.HandleFunc("DELETE /api/runs/{id}", s.handleDelete)
	mux.HandleFunc("POST /api/approve", s.handleApprove)
	mux.HandleFunc("GET /api/about", s.handleAbout)
	mux.HandleFunc("GET /api/workspace", s.handleWorkspace)
	mux.HandleFunc("POST /api/workspace/check", s.handleWorkspaceCheck)
	mux.HandleFunc("POST /api/workspace/pick", s.handleWorkspacePick)
	mux.HandleFunc("POST /api/brief", s.handleBrief)
	mux.HandleFunc("GET /api/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/setup/ollama", s.handleSetupOllama)
	mux.HandleFunc("GET /", s.handleIndex)
	return guard(s.CSRFToken, mux)
}

// claim marks a run busy, or reports that it already is.
//
// Check and set under one lock: two requests that each look, see "free", and
// then proceed is the whole bug this prevents. loop.State is mutated
// throughout Run — messages, steps, cost — and a checkpoint is written per
// tool call, so two turns on one run is both a data race and two writers on
// one file.
//
// Busy is answered with 409 rather than a queue. The refusal already exists in
// the domain: AddUserMessage will not add a message while a batch is
// unfinished. A queue would also mean one browser tab blocking silently behind
// another tab's turn, with no way to tell that from a slow model.
func (s *Server) claim(runID string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[runID] {
		return nil, false
	}
	s.live[runID] = true
	return func() {
		s.mu.Lock()
		delete(s.live, runID)
		s.mu.Unlock()
	}, true
}

// guard enforces the conditions settled on 2026-09-10, restated here because
// they apply per endpoint rather than once at bind time.
//
// Host must be loopback: binding to 127.0.0.1 stops other machines, not other
// pages. Origin, when the browser sends one, must be loopback too — a page on
// any origin can POST to localhost, and it is the Origin header rather than
// the bind address that says who asked. The token then covers what neither
// does, since a page may know the URL without being able to read a response.
func guard(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			httpError(w, http.StatusForbidden, "host is not loopback")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !isLoopbackOrigin(o) {
			httpError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Ariadne-CSRF") != token {
				httpError(w, http.StatusForbidden, "missing or wrong X-Ariadne-CSRF")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port
	}
	h = strings.Trim(h, "[]")
	h = strings.TrimSuffix(h, ".")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}

// sanitiseRunID refuses anything that is not a plain id.
//
// loop.Store has its own traversal guard and this is not a substitute for it;
// it is here so a bad id is a 400 naming the field rather than a 500 from the
// layer below.
func sanitiseRunID(id string) bool {
	return loop.ValidRunID(id)
}
