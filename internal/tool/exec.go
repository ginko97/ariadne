package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
)

// maxExecOutput caps what one exec call returns to the model.
//
// Combined stdout and stderr, first bytes kept. A test run or a build log is
// most useful from the top, where the first failure is, and the size that
// matters is the checkpoint's: fetch without a cap once wedged a run with a
// 66 MB checkpoint, and a verbose command is the same failure with a
// different cause.
const maxExecOutput = 64 << 10

// execWaitDelay bounds how long Run waits after the process is killed.
//
// A child that started its own children can leave them holding the output
// pipes, and Wait blocks until every writer closes. Killing the tree is the
// real fix; this is what stops a tree kill that missed something from hanging
// the run anyway.
const execWaitDelay = 5 * time.Second

// Exec runs a program in the workspace directory.
//
// It takes an argv, never a shell string: what the operator approves is what
// runs, with no quoting, globbing or expansion in between. A pipe is still
// possible by naming a shell as the program, and the approval prompt then
// shows that. Every call is gated — the wiring forces exec into the approval
// list and nothing exempts it.
//
// What Exec does not do is confine. The workspace is the working directory,
// not a boundary: a program run there can open ../anything the user can. The
// controls are the per-call approval and what the process is given — the
// directory, an environment allow-list instead of this process's environment,
// no stdin, a timeout from the loop's tool timeout, and capped output.
type Exec struct {
	dir string
}

func NewExec(workspace string) Exec { return Exec{dir: workspace} }

var _ Tool = Exec{}

func (Exec) Name() string { return "exec" }

func (Exec) Description() string {
	return "Run a program with arguments, in the workspace directory. argv[0] is " +
		"the program, found on PATH; there is no shell, so pipes, redirection, " +
		"globs and built-in commands like dir or cd do not work unless you run a " +
		"shell program explicitly. Every call is shown to the operator for " +
		"approval before it runs. Output is stdout and stderr combined, capped."
}

func (Exec) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "program and its arguments, e.g. [\"go\", \"test\", \"./...\"]"
    }
  },
  "required": ["argv"],
  "additionalProperties": false
}`)
}

type execArgs struct {
	Argv []string `json:"argv"`
}

func (e Exec) Call(ctx context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}

	var in execArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("exec: bad arguments: %v", err)
	}
	if len(in.Argv) == 0 || strings.TrimSpace(in.Argv[0]) == "" {
		return fail("exec: argv must name a program")
	}

	dir, err := filepath.Abs(e.dir)
	if err != nil {
		return fail("exec: workspace: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("exec: workspace: %v", err)
	}

	cmd := exec.CommandContext(ctx, in.Argv[0], in.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = execEnv(os.Environ())
	// nil Stdin is the null device. A program waiting on input would otherwise
	// wait on the operator's terminal, which the loop is also reading.
	cmd.Stdin = nil
	out := &cappedBuffer{max: maxExecOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	killTreeOnCancel(cmd)
	cmd.WaitDelay = execWaitDelay

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started).Round(time.Millisecond)

	var exitErr *exec.ExitError
	switch {
	case errors.Is(runErr, exec.ErrNotFound):
		return fail("exec: %q was not found on PATH. There is no shell: built-in commands "+
			"like dir, cd or echo need a shell program named explicitly.", in.Argv[0])
	case runErr != nil && ctx.Err() != nil:
		// Reported as what happened to the process, with whatever it printed
		// before it was stopped — often the most useful part.
		return llm.ToolResult{
			Content:   fmt.Sprintf("killed after %s: %v\n%s", elapsed, ctx.Err(), out.render()),
			IsError:   true,
			Untrusted: true,
		}, nil
	case runErr != nil && !errors.As(runErr, &exitErr):
		return fail("exec: could not run %q: %v", in.Argv[0], runErr)
	}

	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	// Untrusted: output comes from whatever the program read, and in a
	// workspace that can be a document somebody else wrote.
	return llm.ToolResult{
		Content:   fmt.Sprintf("exit status %d (%s)\n%s", code, elapsed, out.render()),
		IsError:   code != 0,
		Untrusted: true,
	}, nil
}

// execEnvAllowed names the variables a child process keeps.
//
// An allow-list because the agent's own environment holds the provider key:
// dotenv.Load puts .env into it, so an inherited environment would hand
// OPENROUTER_API_KEY to every program the model asks for. A deny-list of
// *KEY*/*TOKEN* names removes only the secrets somebody thought of. What is
// here is what toolchains need to find themselves and somewhere to write.
var execEnvAllowed = map[string]bool{
	// finding programs
	"PATH": true, "PATHEXT": true,
	// Windows itself; much of it will not start without these
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true,
	"PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
	"PROGRAMDATA": true, "COMMONPROGRAMFILES": true,
	"NUMBER_OF_PROCESSORS": true, "PROCESSOR_ARCHITECTURE": true, "OS": true,
	// somewhere to write
	"TEMP": true, "TMP": true, "TMPDIR": true,
	"HOME": true, "USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true,
	"APPDATA": true, "LOCALAPPDATA": true,
	"USER": true, "USERNAME": true, "LANG": true, "LC_ALL": true,
	// Go, which this project's own work runs
	"GOPATH": true, "GOROOT": true, "GOCACHE": true, "GOMODCACHE": true,
	"GOTOOLCHAIN": true, "GOFLAGS": true,
}

func execEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		// Windows keeps per-drive working directories as "=C:" entries; they
		// are not variables anyone set and carry nothing worth passing on.
		if !ok || name == "" {
			continue
		}
		if execEnvAllowed[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return out
}

// cappedBuffer keeps the first max bytes written and counts the rest.
//
// Write never fails. A writer that returned an error at the cap would stop the
// copy from the child's pipe, and a child whose output nobody reads blocks on
// its next write — a verbose command would hang rather than be truncated.
//
// Safe as both Stdout and Stderr of one command: os/exec guarantees at most one
// goroutine writes at a time when the two are the same comparable writer.
type cappedBuffer struct {
	max   int
	buf   []byte
	total int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) render() string {
	if c.total <= int64(len(c.buf)) {
		return string(c.buf)
	}
	return fmt.Sprintf("%s\n[output truncated: %d bytes total, first %d shown]",
		c.buf, c.total, len(c.buf))
}
