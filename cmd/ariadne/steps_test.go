package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// endlessTools is a model that calls calc forever, so a turn ends only at its
// step ceiling, and the number of requests it made is that ceiling.
func endlessTools(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` +
			`{"id":"c","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"1+1\"}"}}]},` +
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A research brief gets room a typed message does not; -max-steps, when
// given, is the operator's number either way.
func TestABriefGetsMoreStepsUnlessMaxStepsIsGiven(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("research something"), 0o644); err != nil {
		t.Fatal(err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })

	for _, c := range []struct {
		name string
		args []string
		want int32
	}{
		{"brief", []string{"-task", brief}, briefMaxSteps},
		{"typed", []string{"research something"}, defaultMaxSteps},
		{"brief with -max-steps", []string{"-max-steps", "3", "-task", brief}, 3},
	} {
		srv, calls := endlessTools(t)
		args := append([]string{"-base-url", srv.URL + "/v1", "-model", "m", "-workspace", t.TempDir()}, c.args...)
		if code := cmdRun(args); code != exitFail {
			t.Errorf("%s: exit %d, want %d (the step limit)", c.name, code, exitFail)
		}
		if got := calls.Load(); got != c.want {
			t.Errorf("%s: %d model calls, want %d", c.name, got, c.want)
		}
	}

	// chat -task builds its agent before the brief's state exists, so the
	// ceiling is set on a separate line there; this is the test for that line.
	empty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	oldIn := os.Stdin
	os.Stdin = empty
	t.Cleanup(func() { os.Stdin = oldIn })
	srv, calls := endlessTools(t)
	cmdChat([]string{"-base-url", srv.URL + "/v1", "-model", "m", "-workspace", t.TempDir(), "-task", brief})
	if got := calls.Load(); got != briefMaxSteps {
		t.Errorf("chat -task: %d model calls, want %d", got, briefMaxSteps)
	}
}
