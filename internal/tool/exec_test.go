package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The child program for these tests is this test binary, re-run with a marker
// argument. That makes the tests portable — no assumption that echo, env or
// sleep exist — and it is a real separate process, which is the thing under
// test.
//
// The marker is an argument, not an environment variable, on purpose: exec
// strips the environment, so a GO_WANT_HELPER variable would never arrive.
const helperMarker = "--exec-helper"

func TestMain(m *testing.M) {
	for i, a := range os.Args {
		if a == helperMarker {
			os.Exit(execHelper(os.Args[i+1:]))
		}
	}
	os.Exit(m.Run())
}

func execHelper(args []string) int {
	switch args[0] {
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "cwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
	case "exit":
		n, _ := strconv.Atoi(args[1])
		fmt.Print("about to fail")
		return n
	case "spew":
		n, _ := strconv.Atoi(args[1])
		os.Stdout.Write([]byte(strings.Repeat("x", n)))
	case "sleep":
		fmt.Println("started")
		time.Sleep(60 * time.Second)
	case "spawn-and-sleep":
		// A grandchild that inherits the output pipe and outlives its parent
		// unless the whole tree is killed.
		self, _ := os.Executable()
		child, err := os.StartProcess(self, []string{self, helperMarker, "sleep"},
			&os.ProcAttr{Files: []*os.File{nil, os.Stdout, os.Stderr}})
		if err != nil {
			fmt.Println("spawn failed:", err)
			return 3
		}
		fmt.Printf("grandchild %d\n", child.Pid)
		time.Sleep(60 * time.Second)
	}
	return 0
}

func runExec(t *testing.T, ctx context.Context, dir string, argv ...string) (string, bool) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{self, helperMarker}, argv...)
	args, _ := json.Marshal(execArgs{Argv: full})
	res, err := NewExec(dir).Call(ctx, "call_1", args)
	if err != nil {
		t.Fatalf("Call returned an error rather than a result: %v", err)
	}
	if !res.Untrusted {
		t.Error("exec output was not marked untrusted")
	}
	return res.Content, res.IsError
}

// The provider key is in this process's environment — dotenv.Load puts it
// there — and must not reach a program the model asked for.
func TestExecDoesNotPassTheAgentsEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-should-not-leak")
	t.Setenv("SOME_TOKEN", "also-not")

	out, isErr := runExec(t, context.Background(), t.TempDir(), "env")
	if isErr {
		t.Fatalf("helper failed:\n%s", out)
	}
	for _, secret := range []string{"sk-or-should-not-leak", "also-not"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q reached the child process:\n%s", secret, out)
		}
	}
	if !strings.Contains(strings.ToUpper(out), "PATH=") {
		t.Errorf("PATH was stripped too; toolchains cannot find themselves:\n%s", out)
	}
}

func TestExecRunsInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	out, _ := runExec(t, context.Background(), dir, "cwd")
	// Resolve both: TempDir can be a short name or behind a symlink.
	want, _ := os.Stat(dir)
	lines := strings.SplitN(out, "\n", 2)
	got, err := os.Stat(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil || !os.SameFile(want, got) {
		t.Errorf("child ran in %q, want the workspace %q", out, dir)
	}
}

// A non-zero exit is a result the model reads, not a failure of the tool, and
// what the program printed before failing comes with it.
func TestExecReportsExitStatusAndOutput(t *testing.T) {
	out, isErr := runExec(t, context.Background(), t.TempDir(), "exit", "7")
	if !isErr {
		t.Error("a non-zero exit was not marked as an error")
	}
	if !strings.Contains(out, "exit status 7") || !strings.Contains(out, "about to fail") {
		t.Errorf("result = %q, want the status and the output", out)
	}
}

func TestExecCapsOutput(t *testing.T) {
	out, _ := runExec(t, context.Background(), t.TempDir(), "spew", strconv.Itoa(maxExecOutput*3))
	if len(out) > maxExecOutput+512 {
		t.Errorf("result is %d bytes, want it capped near %d", len(out), maxExecOutput)
	}
	if !strings.Contains(out, fmt.Sprintf("%d bytes total", maxExecOutput*3)) {
		t.Errorf("truncation was not reported:\n%s", out[len(out)-200:])
	}
}

func TestExecReportsAProgramThatIsNotThere(t *testing.T) {
	args, _ := json.Marshal(execArgs{Argv: []string{"ariadne-no-such-program-xyz"}})
	res, err := NewExec(t.TempDir()).Call(context.Background(), "c", args)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "not found on PATH") {
		t.Errorf("result = %+v, want a not-found error naming PATH", res)
	}
}

func TestExecRefusesAnEmptyArgv(t *testing.T) {
	for _, raw := range []string{`{"argv":[]}`, `{"argv":[""]}`, `{}`} {
		res, _ := NewExec(t.TempDir()).Call(context.Background(), "c", json.RawMessage(raw))
		if !res.IsError {
			t.Errorf("%s was accepted", raw)
		}
	}
}

// A cancelled context stops the process and everything it started, and the
// call returns promptly rather than waiting out the child.
//
// The grandchild holds the output pipe. Without a tree kill, killing only the
// direct child leaves it running and Wait blocked on the pipe until WaitDelay;
// the grandchild would then keep running after the call returned.
func TestExecCancellationKillsTheTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	out, isErr := runExec(t, ctx, t.TempDir(), "spawn-and-sleep")
	elapsed := time.Since(started)

	if !isErr || !strings.Contains(out, "killed after") {
		t.Errorf("result = %q, want a kill report", out)
	}
	if elapsed > 2*time.Second+execWaitDelay-time.Second {
		t.Errorf("call took %s; the tree was not killed, Wait sat out WaitDelay", elapsed)
	}

	var pid int
	for _, line := range strings.Split(out, "\n") {
		if n, ok := strings.CutPrefix(strings.TrimSpace(line), "grandchild "); ok {
			pid, _ = strconv.Atoi(n)
		}
	}
	if pid == 0 {
		t.Fatalf("grandchild pid not reported:\n%s", out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d is still running after the call returned", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
