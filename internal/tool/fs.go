package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	return "Fetch a document by path and return its text content: plain text " +
		"files, and the text of Word, Excel and PowerPoint (.docx, .xlsx, .pptx), " +
		"OpenDocument (.odt, .ods, .odp) and PDF files. Spreadsheets come back as " +
		"tab-separated rows under a heading per sheet. Paths are relative to a " +
		"workspace directory this tool cannot read outside of; absolute paths are refused."
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

func (f Fetch) Call(ctx context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
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

	file, err := root.Open(in.Path)
	if err != nil {
		return fail("fetch: cannot read %q: %v", in.Path, err)
	}
	defer file.Close()

	// A document is read for its text, by its extension; everything else is
	// read as text or refused as binary below. See docs.go.
	switch kind := documentKind(in.Path); kind {
	case "":
	case "legacy":
		return fail("%s", legacyMessage(in.Path))
	default:
		fi, err := file.Stat()
		if err != nil {
			return fail("fetch: cannot read %q: %v", in.Path, err)
		}
		var text string
		if kind == ".pdf" {
			text, err = readPDF(ctx, file, fi.Size())
		} else if fi.Size() > maxDocumentBytes {
			err = fmt.Errorf("it is %d bytes, over the %d-byte limit for documents", fi.Size(), maxDocumentBytes)
		} else {
			text, err = readDocument(file, fi.Size(), kind)
		}
		if err != nil {
			if ctx.Err() != nil {
				return llm.ToolResult{}, ctx.Err()
			}
			return fail("fetch: cannot read %q: %v", in.Path, err)
		}
		// Untrusted, as any fetch: whoever wrote the document is not whoever
		// asked the question.
		return llm.ToolResult{Content: text, Untrusted: true}, nil
	}

	// Format before size, and the order is the point. A PDF is a PDF at any
	// size, but size was checked first, so a 2MB one was reported as too large
	// while a 244KB one — the only one small enough to reach the second check —
	// was reported as not text. Shown that table, a model reasonably concluded
	// the small one was corrupt. Two identical files should not get two
	// different explanations because of which limit they happened to trip.
	//
	// Reading the head is cheap and bounded, so this costs nothing even when the
	// file is enormous.
	head := make([]byte, 8000)
	n, err := file.Read(head)
	if err != nil && err != io.EOF {
		return fail("fetch: cannot read %q: %v", in.Path, err)
	}
	if isBinary(head[:n]) {
		return fail("fetch: %q is not a text document — it looks like binary "+
			"data such as a PDF, image or archive. This tool reads text only; "+
			"a text export of it could be read instead", in.Path)
	}

	// Size is checked before reading the rest rather than after. ReadFile on a
	// 20MB file has already spent the memory by the time anyone can object.
	if fi, err := file.Stat(); err == nil && fi.Size() > maxFetchBytes {
		return fail("fetch: %q is %d bytes, over the %d-byte limit; "+
			"a document this large cannot be read into the conversation",
			in.Path, fi.Size(), maxFetchBytes)
	}

	rest, err := io.ReadAll(io.LimitReader(file, maxFetchBytes-int64(n)))
	if err != nil {
		return fail("fetch: cannot read %q: %v", in.Path, err)
	}
	data := append(head[:n], rest...)

	// Untrusted: whoever wrote this document is not whoever asked the question.
	return llm.ToolResult{Content: string(data), Untrusted: true}, nil
}

// maxFetchBytes bounds one document.
//
// A tool result is not a file, it is a message that joins the conversation and
// is resent with every subsequent request — so an unbounded read is an
// unbounded prompt. A 20MB PDF fetched once produced a 66MB checkpoint, a 400
// from the provider on the very next request, and a run that was checkpointed,
// resumable and permanently unable to finish: compaction could not rescue it
// either, because the keep-floor protects the most recent exchange, which was
// the 20MB message.
//
// 256KB is far past any text document worth summarising and far short of what
// breaks a context window.
const maxFetchBytes = 256 << 10

// isBinary reports whether data looks like something no model can read.
//
// A NUL byte in the first few kilobytes, which is the heuristic git uses and is
// right for the same reason: text files do not contain them and binary formats
// almost always do within a header. Reading a PDF as a string does not fail, it
// succeeds at producing several million characters of compressed rubbish that
// costs tokens and answers nothing.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
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
	return "Write text to a file, creating it or replacing its contents. " +
		"Paths are relative to a workspace directory this tool cannot write " +
		"outside of; absolute paths are refused."
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
