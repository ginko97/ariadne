package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// `ariadne run` says, after the answer, which URLs it cited without opening
// them — on stderr, so `> answer.txt` still holds only the answer.
func TestRunNamesCitedURLsItNeverOpened(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"QRIS is free (https://guessed.example/qris-fees)."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`))
	}))
	t.Cleanup(srv.Close)

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	stdout, stderr := make(chan string, 1), make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); stdout <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); stderr <- string(b) }()

	code := cmdRun([]string{"-base-url", srv.URL + "/v1", "-model", "m", "-workspace", t.TempDir(), "is QRIS free?"})
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, errText := <-stdout, <-stderr

	if code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, errText)
	}
	if !strings.Contains(errText, "cited but never opened in this conversation (1):\n  https://guessed.example/qris-fees\n") {
		t.Errorf("stderr does not name the unopened URL:\n%s", errText)
	}
	if strings.Contains(out, "never opened") {
		t.Errorf("the note went to stdout, which is the answer's:\n%s", out)
	}
}
