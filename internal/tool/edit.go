package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// EditFileName is the tool's name, exported because the wiring gates it by
// default and -trust names it.
const EditFileName = "edit_file"

// maxEditBytes bounds the files edit_file will open. Larger than fetch's cap,
// because the file is not sent to the model — only the replacement is — but
// still a bound: a multi-megabyte file is not something to rewrite on a
// model's say-so.
const maxEditBytes = 4 << 20

// EditFile replaces an exact piece of text in a file, inside the same sandbox
// as fetch and write_file.
//
// It exists because whole-file write_file is how models damage files: to change
// one line they must reproduce every other line, and whatever they drop or
// paraphrase is gone. Here the model names the text to change, and everything
// it did not name is left byte for byte as it was.
//
// The match must be exact and, unless replace_all is set, unique. An ambiguous
// or missing match is refused with how many times the text occurred, so the
// model can widen its snippet rather than edit the wrong place.
//
// The write is atomic: the new content goes to a temporary file in the same
// directory, takes the original's permissions, and is renamed over it — a crash
// leaves the old file, never half of a new one.
type EditFile struct{ sandbox }

func NewEditFile(root string) EditFile { return EditFile{sandbox{root}} }

var _ Tool = EditFile{}

func (EditFile) Name() string { return EditFileName }

func (EditFile) Description() string {
	return "Replace an exact piece of text in an existing file, leaving the rest of " +
		"the file untouched. old_text must match the file exactly, including " +
		"whitespace and indentation, and must occur exactly once unless replace_all " +
		"is true — include enough surrounding lines to make it unique. To create a " +
		"file, use write_file. Paths are relative to the workspace; this tool " +
		"cannot edit outside it."
}

func (EditFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":        {"type": "string", "description": "path of the file to edit, relative to the workspace"},
    "old_text":    {"type": "string", "description": "the exact text to replace"},
    "new_text":    {"type": "string", "description": "the text to put in its place"},
    "replace_all": {"type": "boolean", "description": "replace every occurrence instead of requiring exactly one"}
  },
  "required": ["path", "old_text", "new_text"],
  "additionalProperties": false
}`)
}

type editArgs struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all"`
}

func (e EditFile) Call(_ context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}

	var in editArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("edit_file: bad arguments: %v", err)
	}
	switch {
	case in.Path == "":
		return fail("edit_file: path is required")
	case in.OldText == "":
		return fail("edit_file: old_text is empty; to create a file or replace all of it, use write_file")
	case in.OldText == in.NewText:
		return fail("edit_file: old_text and new_text are the same; nothing would change")
	}

	root, err := e.open()
	if err != nil {
		return fail("edit_file: %v", err)
	}
	defer root.Close()

	info, err := root.Stat(in.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail("edit_file: %s does not exist; to create it, use write_file", in.Path)
	case err != nil:
		return fail("edit_file: %v", err)
	case !info.Mode().IsRegular():
		return fail("edit_file: %s is not a regular file", in.Path)
	case info.Size() > maxEditBytes:
		return fail("edit_file: %s is %d bytes; edit_file works on files up to %d", in.Path, info.Size(), maxEditBytes)
	}

	data, err := root.ReadFile(in.Path)
	if err != nil {
		return fail("edit_file: %v", err)
	}
	if isBinary(data) {
		return fail("edit_file: %s is not a text file", in.Path)
	}
	content := string(data)

	m := resolveEdit(content, in.OldText, in.NewText)
	oldText, newText, n := m.Old, m.New, m.Count
	switch {
	case n == 0:
		return fail("edit_file: old_text was not found in %s; it must match exactly, including whitespace and indentation", in.Path)
	case n > 1 && !in.ReplaceAll:
		return fail("edit_file: old_text occurs %d times in %s; include more surrounding text to make it unique, or set replace_all", n, in.Path)
	}

	line := m.Line
	count := 1
	if in.ReplaceAll {
		count = -1
	}
	updated := strings.Replace(content, oldText, newText, count)

	if err := writeAtomically(root, in.Path, []byte(updated), info.Mode().Perm()); err != nil {
		return fail("edit_file: %v", err)
	}
	if in.ReplaceAll && n > 1 {
		return llm.ToolResult{Content: fmt.Sprintf("edited %s: replaced %d occurrences, the first at line %d", in.Path, n, line)}, nil
	}
	return llm.ToolResult{Content: fmt.Sprintf("edited %s: replaced 1 occurrence at line %d", in.Path, line)}, nil
}

// editMatch is where an edit_file call lands: the text that will actually be
// replaced, the replacement in the file's own line endings, how many times it
// occurs, and the 1-based line of the first occurrence.
//
// Separate from Call because the approval preview has to show the operator the
// same match the tool will make. A preview that finds a match the tool then
// refuses, or that shows LF where the file has CRLF, is worse than no preview.
type editMatch struct {
	Old, New string
	Count    int
	Line     int
}

// resolveEdit finds old_text in content, forgiving the two ways a model's copy
// of a file differs from the file it read.
func resolveEdit(content, oldText, newText string) editMatch {
	n := strings.Count(content, oldText)
	// A model writes "\n"; a file saved on Windows has "\r\n". When the text
	// only matches with the file's own line endings, both sides are converted,
	// so an edit to a CRLF file keeps it CRLF instead of leaving mixed endings.
	if n == 0 && strings.Contains(content, "\r\n") && !strings.Contains(oldText, "\r\n") {
		crlfOld := strings.ReplaceAll(oldText, "\n", "\r\n")
		if m := strings.Count(content, crlfOld); m > 0 {
			oldText, newText, n = crlfOld, strings.ReplaceAll(newText, "\n", "\r\n"), m
		}
	}
	// A model that has just read the file often hands back its text with one
	// newline more than the file holds — the fence used to add one, and a
	// document's last line is easy to mis-copy. The match is still exact; only
	// trailing newlines are reconsidered, and the same ones come off new_text
	// so the replacement keeps what followed.
	if n == 0 {
		if trimmed := strings.TrimRight(oldText, "\r\n"); trimmed != oldText && trimmed != "" {
			if m := strings.Count(content, trimmed); m > 0 {
				suffix := oldText[len(trimmed):]
				newTrimmed := newText
				if strings.HasSuffix(newText, suffix) {
					newTrimmed = newText[:len(newText)-len(suffix)]
				}
				oldText, newText, n = trimmed, newTrimmed, m
			}
		}
	}
	// The replacement takes the file's line endings. A model writes "\n" even
	// when the text it was shown has "\r\n", so a new line inserted into a
	// CRLF file would otherwise be the one line in it ending differently.
	if strings.Contains(content, "\r\n") {
		newText = strings.ReplaceAll(strings.ReplaceAll(newText, "\r\n", "\n"), "\n", "\r\n")
	}
	match := editMatch{Old: oldText, New: newText, Count: n}
	if n > 0 {
		match.Line = strings.Count(content[:strings.Index(content, oldText)], "\n") + 1
	}
	return match
}

// rootFS is what writeAtomically needs from an *os.Root.
type rootFS interface {
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Chmod(name string, mode fs.FileMode) error
	Rename(oldname, newname string) error
	Remove(name string) error
}

// writeAtomically replaces name with data through a temporary file in the same
// directory, confined by the same root.
func writeAtomically(root rootFS, name string, data []byte, perm fs.FileMode) error {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := filepath.Join(filepath.Dir(name), ".ariadne-edit-"+hex.EncodeToString(suffix[:]))
	if err := root.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	// WriteFile's perm is filtered by the umask; the edited file should keep
	// exactly the permissions it had.
	if err := root.Chmod(tmp, perm); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
