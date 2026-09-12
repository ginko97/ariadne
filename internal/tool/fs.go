package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// sandbox confines a tool to one directory.
//
// Every path a tool is given comes from the model, and everything the model
// sees may have been written by someone else — a fetched page, a previous tool
// result. So a path argument is untrusted input in the ordinary security sense,
// and the only safe assumption is that it will eventually contain "..".
type sandbox struct{ root string }

// resolve maps a model-supplied path to an absolute one inside the sandbox.
//
// Clean("/"+rel) is the load-bearing part: rooting the path first collapses any
// leading "..", so "../../etc/passwd" becomes "/etc/passwd" and then joins under
// root rather than escaping it. The check afterwards is belt and braces, and
// catches symlinks that Clean cannot see.
func (s sandbox) resolve(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}

	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("bad sandbox root: %w", err)
	}

	abs := filepath.Join(root, filepath.Clean("/"+rel))

	// EvalSymlinks on a path that does not exist yet fails, so only resolve the
	// parent, which must exist for a write and does exist for a read.
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(resolved, filepath.Base(abs))
	}

	rootWithSep := root + string(filepath.Separator)
	if abs != root && !strings.HasPrefix(abs, rootWithSep) {
		return "", fmt.Errorf("path %q escapes the sandbox", rel)
	}
	return abs, nil
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
	abs, err := f.resolve(in.Path)
	if err != nil {
		return fail("fetch: %v", err)
	}
	data, err := os.ReadFile(abs)
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
	abs, err := w.resolve(in.Path)
	if err != nil {
		return fail("write_file: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail("write_file: %v", err)
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o600); err != nil {
		return fail("write_file: %v", err)
	}
	return llm.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)}, nil
}
