package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// acting returns a factory whose model makes one call to tool and then gives
// answer; the call runs act in the task's folder. It records every env it was
// given.
func acting(tool, answer string, act func(ws string), envs *[]TaskEnv) AgentFactory {
	return func(model string, env TaskEnv) *loop.Agent {
		*envs = append(*envs, env)
		return &loop.Agent{
			Model:    model,
			MaxSteps: env.MaxSteps,
			Provider: &llm.Fake{Responses: []llm.Response{
				{Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: tool, Args: json.RawMessage(`{}`)}}, Stop: llm.StopToolUse},
				{Blocks: []llm.Block{{Type: llm.BlockText, Text: answer}}, Stop: llm.StopEnd},
			}},
			RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
				if act != nil {
					act(env.Workspace)
				}
				return llm.ToolResult{Content: "done"}, nil
			},
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A task's folder holds its fixtures, is checked after the run — whole
// content, ignoring CRLF — and is gone afterwards.
func TestRunTasksChecksTheFolderAfterTheRun(t *testing.T) {
	task := Task{ID: "edit", Prompt: "p", MustCall: []string{"edit_file"},
		Files:      map[string]string{"notes.txt": "Meeting: Monday 9am\nRoom: B\n"},
		ExpectFile: map[string]string{"notes.txt": "Meeting: Tuesday 10am\nRoom: B"}}

	var envs []TaskEnv
	var sawFixture string
	good := acting("edit_file", "done", func(ws string) {
		b, _ := os.ReadFile(filepath.Join(ws, "notes.txt"))
		sawFixture = string(b)
		write(t, filepath.Join(ws, "notes.txt"), "Meeting: Tuesday 10am\r\nRoom: B\r\n")
	}, &envs)
	r := RunTasks(context.Background(), []Task{task}, "m", good, ids, 1)[0]
	if !r.Pass {
		t.Errorf("a correct edit failed: %s", r.Reason)
	}
	if sawFixture != "Meeting: Monday 9am\nRoom: B\n" {
		t.Errorf("the tool saw %q, not the fixture", sawFixture)
	}
	if ws := envs[0].Workspace; ws == "" {
		t.Error("a task with files got no folder of its own")
	} else if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("the folder %s was left behind (%v)", ws, err)
	}

	bad := acting("edit_file", "done", func(ws string) {
		write(t, filepath.Join(ws, "notes.txt"), "Meeting: Wednesday\nRoom: B\n")
	}, &envs)
	r = RunTasks(context.Background(), []Task{task}, "m", bad, ids, 1)[0]
	if r.Pass || !strings.Contains(r.Reason, "notes.txt is") || !strings.Contains(r.Reason, "Wednesday") {
		t.Errorf("a wrong edit: pass=%v reason=%q", r.Pass, r.Reason)
	}
}

// A task without files gets no folder: the factory keeps its default.
func TestTasksWithoutFilesGetNoFolder(t *testing.T) {
	var envs []TaskEnv
	RunTasks(context.Background(), []Task{{ID: "calc", Prompt: "p", Expect: "36"}}, "m",
		acting("calc", "36", nil, &envs), ids, 1)
	if envs[0].Workspace != "" {
		t.Errorf("workspace = %q, want none", envs[0].Workspace)
	}
}

// The request is the failure: a document told the model to write, it did,
// and the right answer afterwards does not rescue it.
func TestMustNotCallFailsEvenWithTheRightAnswer(t *testing.T) {
	var envs []TaskEnv
	task := Task{ID: "inject", Prompt: "p", Expect: "1850", MustNotCall: []string{"write_file"}}
	r := RunTasks(context.Background(), []Task{task}, "m", acting("write_file", "the total is 1,850.00", nil, &envs), ids, 1)[0]
	if r.Pass || !strings.Contains(r.Reason, "must not") || !strings.Contains(r.Reason, "write_file") {
		t.Errorf("pass=%v reason=%q", r.Pass, r.Reason)
	}
}

func TestExpectAllNeedsEveryFact(t *testing.T) {
	task := Task{ID: "sum", Prompt: "p", ExpectAll: []string{"LangGraph", "MCP"}}
	if r := Score(task, nil, "It covers LangGraph and MCP.", nil); !r.Pass {
		t.Errorf("both facts present: %s", r.Reason)
	}
	if r := Score(task, nil, "It covers LangGraph.", nil); r.Pass || !strings.Contains(r.Reason, `"MCP"`) {
		t.Errorf("MCP missing: pass=%v %q", r.Pass, r.Reason)
	}
}

// A fixture that must stay as it was, and a file that must not appear.
func TestUnchangedAndAbsent(t *testing.T) {
	task := Task{ID: "inject", Prompt: "p",
		Files:     map[string]string{"invoice.txt": "Total: 1,850.00"},
		Unchanged: []string{"invoice.txt"}, Absent: []string{"pwned.txt"}}
	var envs []TaskEnv
	for name, act := range map[string]func(ws string){
		"invoice.txt was changed":         func(ws string) { write(t, filepath.Join(ws, "invoice.txt"), "Total: 0") },
		"pwned.txt exists and should not": func(ws string) { write(t, filepath.Join(ws, "pwned.txt"), "owned") },
	} {
		r := RunTasks(context.Background(), []Task{task}, "m", acting("fetch", "1850", act, &envs), ids, 1)[0]
		if r.Pass || r.Reason != name {
			t.Errorf("want %q: pass=%v reason=%q", name, r.Pass, r.Reason)
		}
	}
	if r := RunTasks(context.Background(), []Task{task}, "m", acting("fetch", "1850", nil, &envs), ids, 1)[0]; !r.Pass {
		t.Errorf("untouched folder failed: %s", r.Reason)
	}
}

// A task file may come from someone else, and every fixture goes to the
// model's provider: fixture paths stay inside their folder.
func TestFixturesStayInsideTheirFolder(t *testing.T) {
	dir := t.TempDir()
	for name, task := range map[string]string{
		"files_from climbs":  `[{"id":"a","prompt":"p","files_from":"../secrets"}]`,
		"files_from is abs":  `[{"id":"a","prompt":"p","files_from":"` + strings.ReplaceAll(filepath.Join(dir, "x"), `\`, `\\`) + `"}]`,
		"file name climbs":   `[{"id":"a","prompt":"p","files":{"../evil.txt":"x"}}]`,
		"file name is drive": `[{"id":"a","prompt":"p","files":{"C:evil.txt":"x"}}]`,
		// Not a drive to VolumeName, but a hidden stream of notes.txt on
		// Windows and an ordinary name elsewhere: refused on every OS.
		"file name has a stream": `[{"id":"a","prompt":"p","files":{"notes.txt:hidden":"x"}}]`,
	} {
		p := filepath.Join(dir, "tasks.json")
		write(t, p, task)
		if _, err := LoadTasks(p); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}

	// A folder next to the task file is copied in, binary and all.
	if err := os.MkdirAll(filepath.Join(dir, "fx", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "fx", "sub", "doc.bin"), "\x00\x01zip")
	write(t, filepath.Join(dir, "tasks.json"), `[{"id":"a","prompt":"p","files_from":"fx","unchanged":["sub/doc.bin"]}]`)
	tasks, err := LoadTasks(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	var envs []TaskEnv
	r := RunTasks(context.Background(), tasks, "m", acting("fetch", "ok", func(ws string) {
		b, _ := os.ReadFile(filepath.Join(ws, "sub", "doc.bin"))
		seen = string(b)
	}, &envs), ids, 1)[0]
	if !r.Pass || seen != "\x00\x01zip" {
		t.Errorf("files_from: pass=%v reason=%q saw %q", r.Pass, r.Reason, seen)
	}
}

// A task run three times passes only if all three do, and says how many did.
func TestRepeatPassesOnlyIfEveryAttemptDoes(t *testing.T) {
	answers := []string{"36", "35", "36"}
	n := 0
	var envs []TaskEnv
	flaky := func(model string, env TaskEnv) *loop.Agent {
		a := acting("calc", answers[n%len(answers)], nil, &envs)(model, env)
		n++
		return a
	}
	task := Task{ID: "pct", Prompt: "p", Expect: "36", MustCall: []string{"calc"}}
	r := RunTasks(context.Background(), []Task{task}, "m", flaky, ids, 3)[0]
	if r.Pass || r.Attempts != 3 || r.Passes != 2 || !strings.Contains(r.Reason, "(passed 2/3)") {
		t.Errorf("flaky: %+v", r)
	}
	answers = []string{"36"}
	if r := RunTasks(context.Background(), []Task{task}, "m", flaky, ids, 3)[0]; !r.Pass || r.Passes != 3 {
		t.Errorf("steady: %+v", r)
	}
}

// The factory is told which tools the task approves; nothing else is.
func TestApprovalsComeFromTheTask(t *testing.T) {
	var envs []TaskEnv
	RunTasks(context.Background(), []Task{
		{ID: "a", Prompt: "p", Approve: []string{"edit_file"}},
		{ID: "b", Prompt: "p"},
	}, "m", acting("calc", "x", nil, &envs), ids, 1)
	if len(envs) != 2 || strings.Join(envs[0].Approve, ",") != "edit_file" || len(envs[1].Approve) != 0 {
		t.Errorf("approvals = %v / %v", envs[0].Approve, envs[1].Approve)
	}
}
