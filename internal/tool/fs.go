package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ginko97/ariadne/internal/llm"
)

// sandbox confines a tool to one directory.
//
// Every path a tool is given comes from the model, and everything the model
// sees may have been written by someone else — a fetched page, a previous tool
// result. So a path argument is untrusted input in the ordinary security sense,
// and the only safe assumption is that it will eventually contain "..".
//
// Confinement is os.Root's job rather than ours, and that is the whole design.
// An earlier version did it lexically: clean the path, resolve symlinks, then
// check the result still has the root as a prefix. A Windows directory junction
// — which `mklink /J` creates with no privileges at all — walked straight
// through that. filepath.EvalSymlinks returns a junction *unchanged* with a nil
// error, and errors outright on a path leading through one, so both branches
// left the path unresolved and the lexical check then passed on a path the OS
// would happily follow out of the tree. Read and write both escaped.
//
// os.Root enforces the boundary per path component at the syscall layer, so a
// reparse point that leaves the tree is refused there rather than argued about
// here. The lesson is worth more than the fix: path arithmetic describes what a
// name looks like, and confinement is a question about what the filesystem will
// do with it. Only the kernel knows the second one.
//
// One deliberate behaviour change: absolute paths are now refused rather than
// remapped inside the root. Silently rewriting "/etc/passwd" to something else
// was never what the caller meant, and a refusal the model can read is better
// than a surprise it cannot.
type sandbox struct{ root string }

// open returns a handle confined to the sandbox root.
//
// Opened per call rather than held on the struct: the tools stay values with no
// lifecycle and nothing to close, and a root that is moved or replaced between
// calls cannot leave a stale handle pointing at wherever it went.
func (s sandbox) open() (*os.Root, error) {
	return os.OpenRoot(s.root)
}

// openForWrite is open, plus creating the root if this is a run's first write.
// A job should not fail because nothing has made the directory yet.
func (s sandbox) openForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(s.root)
}

// ---------------------------------------------------------------- fetch

// Fetch reads a document and returns its text.
//
// It reads from a directory rather than the network, which is enough to study
// what happens when text somebody else wrote enters the conversation, without
// also handing the agent the ability to reach arbitrary hosts.
type Fetch struct{ sandbox }

func NewFetch(root string) Fetch { return Fetch{sandbox{root}} }

var _ Tool = Fetch{}

func (Fetch) Name() string { return "fetch" }

func (Fetch) Description() string {
	return "Fetch a document by path and return its text content."
}

func (Fetch) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "path of the document to fetch"}
  },
  "required": ["path"],
  "additionalProperties": false
}`)
}

type fetchArgs struct {
	Path string `json:"path"`
}

func (f Fetch) Call(_ context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}

	var in fetchArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("fetch: bad arguments: %v", err)
	}
	if in.Path == "" {
		return fail("fetch: path is required")
	}

	root, err := f.open()
	if err != nil {
		return fail("fetch: %v", err)
	}
	defer root.Close()

	data, err := root.ReadFile(in.Path)
	if err != nil {
		return fail("fetch: cannot read %q: %v", in.Path, err)
	}
	// Untrusted: whoever wrote this document is not whoever asked the question.
	return llm.ToolResult{Content: string(data), Untrusted: true}, nil
}

// ---------------------------------------------------------------- write_file

// WriteFile creates or replaces a file.
//
// The first tool here with a side effect, which makes it the first tool where
// callID matters: a replayed step must not write twice. Writing the same bytes
// to the same path twice is harmless, so this one is naturally idempotent — a
// tool that appended, sent mail or moved money would have to use the id.
type WriteFile struct{ sandbox }

func NewWriteFile(root string) WriteFile { return WriteFile{sandbox{root}} }

var _ Tool = WriteFile{}

func (WriteFile) Name() string { return "write_file" }

func (WriteFile) Description() string {
	return "Write text to a file, creating it or replacing its contents."
}

func (WriteFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string", "description": "path of the file to write"},
    "content": {"type": "string", "description": "text to write"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (w WriteFile) Call(_ context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}

	var in writeArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("write_file: bad arguments: %v", err)
	}
	if in.Path == "" {
		return fail("write_file: path is required")
	}

	root, err := w.openForWrite()
	if err != nil {
		return fail("write_file: %v", err)
	}
	defer root.Close()

	// filepath.Dir takes either separator on Windows, so "notes/todo.txt" and
	// "notes\todo.txt" reach the same directory. MkdirAll is confined by the
	// same root, so a parent that escapes is refused here rather than created
	// first and written into afterwards.
	if dir := filepath.Dir(in.Path); dir != "." && dir != string(filepath.Separator) {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fail("write_file: %v", err)
		}
	}
	if err := root.WriteFile(in.Path, []byte(in.Content), 0o600); err != nil {
		return fail("write_file: %v", err)
	}
	return llm.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)}, nil
}
