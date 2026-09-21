package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/memory"
)

// defaultModelFor picks a model that belongs to the endpoint being used.
//
// Explicit flag beats ARIADNE_MODEL beats this, so nothing anybody typed is
// overridden — it only decides what a bare command means. A gateway that
// namespaces its ids has no use for a bare one, and the reverse is equally
// true.
func defaultModelFor(baseURL string) string {
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
	switch {
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		return defaultOpenRouterModel
	case host == "openai.com" || strings.HasSuffix(host, ".openai.com"):
		return defaultOpenAIModel
	case host == "x.ai" || strings.HasSuffix(host, ".x.ai"):
		return defaultXAIModel
	case host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com"):
		return defaultModel
	default:
		return defaultModel
	}
}

// memoryPrompt renders the notes earlier runs left, fenced.
//
// Appended to the system prompt rather than injected as a message, so
// State.System records exactly what this run was told and a checkpoint says
// which notes it saw. Read once at the start: memory that changed under a
// running agent would mean two steps of the same run disagreeing about what is
// remembered.
//
// A failure here is a warning, not a fatal error. A run that cannot read its
// notes is a run with no notes, which is the state every first run is in, and
// refusing to work because a scratch file is unreadable would be the wrong
// trade.
func memoryPrompt(on bool) string {
	if !on {
		return ""
	}
	block, err := (memory.Store{Path: memoryFile}).Prompt()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ariadne: memory unreadable, continuing without it: %v\n", err)
		return ""
	}
	if block == "" {
		return ""
	}
	return "\n\n" + block
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// checkResumeGrants refuses a resume whose memory setting the allow-lists
// cannot support.
//
// Two lists matter and they fail differently. The flag list is the operator's
// sentence now; the checkpoint's list is the grant the run has been operating
// under, and this project never widens one. Either way the failure to avoid is
// silent: memory read on with the write tool refused at the loop, which looks
// like a model that will not use a tool it can see.
func checkResumeGrants(mem bool, allowFlag []string, st *loop.State) error {
	if !mem {
		return nil
	}
	if len(allowFlag) > 0 && !contains(allowFlag, "remember") {
		return errors.New("memory with -allow needs remember in the list")
	}
	if len(st.Allow) > 0 && !contains(st.Allow, "remember") {
		return errors.New("the checkpoint's allow-list has no remember, and resume cannot widen it")
	}
	return nil
}

// conversationEndpoint picks where a UI conversation's requests go, and with
// which key.
//
// A conversation recorded against another endpoint keeps it, the same rule
// resume follows. It takes that endpoint's key or none, never the server's:
// d9e9778 fell back to the server's key when the right one was unset, which
// sent an OpenRouter key to api.x.ai for a conversation recorded there with no
// XAI_API_KEY. With no key the turn fails with the provider's 401, shown in the
// page, and nothing is sent anywhere it should not go.
//
// Unlike resume, an explicit -base-url does not override the recorded
// endpoint: the server cannot tell a flag from its default, and one server
// holds many conversations that were not all started against it.
func conversationEndpoint(serverURL, serverKey string, st *loop.State) (url, key string) {
	if st == nil || st.BaseURL == "" || st.BaseURL == serverURL {
		return serverURL, serverKey
	}
	k, envName := apiKey(st.BaseURL)
	if k == "" {
		fmt.Fprintf(os.Stderr, "warning: a conversation uses %s and there is no key for it (set %s); its turns will fail\n", st.BaseURL, envName)
	}
	return st.BaseURL, k
}

// resolveEndpoint picks the provider for a resumed run, recording an explicit
// override.
//
// The checkpoint wins by default: a job that finishes somewhere other than it
// started is a different job. A flag is the operator saying otherwise, and that
// belongs on the state too — the rest of the run really does go elsewhere, and
// the next resume should know.
func resolveEndpoint(flag string, st *loop.State) string {
	if flag != "" {
		st.BaseURL = flag
		return flag
	}
	if st.BaseURL != "" {
		return st.BaseURL
	}
	return envOr("ARIADNE_BASE_URL", defaultBaseURL)
}

// resolveWorkspace picks the directory a resumed run's file tools are confined
// to, recording an explicit override.
//
// The checkpoint wins by default, for the reason every other resume field does:
// a run that read one repository and resumes against another has every path it
// remembers pointing at files that are not the ones it saw. A flag is the
// operator saying otherwise, and that belongs on the state too, because the
// rest of the run really does happen elsewhere.
func resolveWorkspace(flag string, st *loop.State) string {
	if flag != "" {
		st.Workspace = flag
		return flag
	}
	if st.Workspace != "" {
		return st.Workspace
	}
	st.Workspace = defaultWorkspace
	return defaultWorkspace
}

// resolveBudget picks the context budget for a resumed run, recording an
// explicit override.
//
// Recording is the whole point. The loop reads State.ContextBudget, and Run
// only seeds it when it is zero — so a checkpoint carrying 5000 silently
// ignored a `-context-budget 3000` on the command line and kept compacting
// against the old number. The flag was accepted, printed in no error, and did
// nothing.
func resolveBudget(flag int, st *loop.State) int {
	if flag > 0 {
		st.ContextBudget = flag
		return flag
	}
	return st.ContextBudget
}

// resolveModel picks the model for a resumed chat, recording an explicit
// CLI override if provided.
func resolveModel(flag string, explicit bool, st *loop.State) string {
	if explicit && flag != "" {
		st.Model = flag
		return flag
	}
	if st.Model != "" {
		return st.Model
	}
	return flag
}
