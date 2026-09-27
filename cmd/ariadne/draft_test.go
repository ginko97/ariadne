package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ginko97/ariadne/internal/loop"
)

// Drafting a task file offers the model no tool at all, under the drafting
// prompt, and records the draft as a conversation. What comes back is the
// file's text without a code fence around it.
func TestDraftTaskOffersNoTools(t *testing.T) {
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` +
			"```markdown\\n# Weekly IHSG\\n\\nRead idx.co.id.\\n\\nGive the result as your answer.\\n```" +
			`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`))
	}))
	t.Cleanup(srv.Close)

	store := &loop.Store{Dir: runsDir}
	opts := agentOpts{Key: "k", Model: "m", BaseURL: srv.URL + "/v1", RunID: "run_20260925T000000_draft1", MaxSteps: 10,
		Exec: true, Store: store}
	folder := t.TempDir()
	text, err := draftTask(context.Background(), opts, folder, "weekly IHSG report")
	if err != nil {
		t.Fatal(err)
	}
	if text != "# Weekly IHSG\n\nRead idx.co.id.\n\nGive the result as your answer.\n" {
		t.Errorf("draft = %q", text)
	}
	if len(bodies) != 1 {
		t.Fatalf("%d model calls, want 1", len(bodies))
	}
	if tools, ok := bodies[0]["tools"]; ok && tools != nil {
		t.Errorf("the drafting request offered tools: %v", tools)
	}
	raw, _ := json.Marshal(bodies[0]["messages"])
	if !strings.Contains(string(raw), "drafting a task file") || !strings.Contains(string(raw), "weekly IHSG report") {
		t.Errorf("request messages lack the drafting prompt or the description: %s", raw)
	}
	st, err := store.Load(opts.RunID)
	if err != nil || st.Workspace != folder || !strings.HasPrefix(st.Task, "Draft a task file: ") {
		t.Errorf("the draft is not on record as a conversation: %+v, %v", st, err)
	}
}

func TestUnfenceTakesOffOneOuterFence(t *testing.T) {
	for in, want := range map[string]string{
		"```markdown\n# T\n\nbody\n```":             "# T\n\nbody\n",
		"```\n# T\n```":                             "# T\n",
		"# T\n\n```bash\nls\n```":                   "# T\n\n```bash\nls\n```\n",
		"  # T  ":                                   "# T\n",
		"````markdown\n# T\n```bash\nls\n```\n````": "# T\n```bash\nls\n```\n",
		"````\n# T\n````":                           "# T\n",
		"````markdown\n# T\nSee `code`\n````":       "# T\nSee `code`\n",
	} {
		if got := unfence(in); got != want {
			t.Errorf("unfence(%q) = %q, want %q", in, got, want)
		}
	}
}
