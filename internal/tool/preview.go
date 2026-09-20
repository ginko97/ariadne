package tool

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
)

// previewLines bounds how much of a file's new content a card shows. Long
// enough to recognise what is being written, short enough that the buttons stay
// on screen — an approval you have to scroll past is one you stop reading.
const previewLines = 20

// Line is one line of an approval preview, with what it means to the operator
// rather than how to style it.
type Line struct {
	// Kind is "plain", "added" (text that will appear), "removed" (text that
	// will go), or "warn" (the call will be refused, or something is off).
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func plain(format string, a ...any) Line { return Line{Kind: "plain", Text: fmt.Sprintf(format, a...)} }
func warn(format string, a ...any) Line  { return Line{Kind: "warn", Text: fmt.Sprintf(format, a...)} }

// Preview says, in words, what one tool call would do, for the person deciding
// whether to allow it.
//
// It exists because both front ends printed the raw JSON arguments, which reads
// as {"new_text":"...","path":"...","old_text":"..."} — approvable on three
// lines, unreadable on a real file, and a gate nobody reads is not a gate.
//
// Read-only and best-effort: it opens the workspace to say what a write would
// replace, and any failure there degrades to describing the arguments rather
// than refusing to draw a card. Nothing here reaches the model — a preview is
// shown to the operator and then discarded.
func Preview(name string, args json.RawMessage, workspace string) []Line {
	switch name {
	case WriteFileName:
		var in writeArgs
		if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
			break
		}
		return previewWrite(in, workspace)
	case EditFileName:
		var in editArgs
		if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
			break
		}
		return previewEdit(in, workspace)
	case WebFetchName:
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args, &in); err != nil || in.URL == "" {
			break
		}
		lines := []Line{plain("GET %s", in.URL)}
		if u, err := url.Parse(in.URL); err == nil && u.Host != "" {
			lines = append(lines, plain("host: %s", u.Host))
		}
		return lines
	case "exec":
		var in execArgs
		if err := json.Unmarshal(args, &in); err != nil || len(in.Argv) == 0 {
			break
		}
		lines := []Line{plain("run %s", in.Argv[0])}
		for _, a := range in.Argv[1:] {
			lines = append(lines, plain("  %s", a))
		}
		return append(lines,
			plain("in %s", workspace),
			warn("exec is not confined to that folder"))
	case "remember":
		var in rememberArgs
		if err := json.Unmarshal(args, &in); err != nil || in.Note == "" {
			break
		}
		// The note, not the JSON: a planted note is the one thing the
		// postmortem measured a later run obeying, so the whole value of this
		// card is reading the sentence that will be kept.
		return []Line{plain("remember: %s", in.Note)}
	}
	return describeArgs(args)
}

// previewWrite reports whether the file exists and what is about to replace it.
func previewWrite(in writeArgs, workspace string) []Line {
	lines := []Line{}
	switch existing, err := readForPreview(workspace, in.Path); {
	case err != nil:
		lines = append(lines, plain("creates %s", in.Path))
	case existing == in.Content:
		lines = append(lines, plain("rewrites %s with what it already holds", in.Path))
	default:
		lines = append(lines, plain("replaces %s — %d lines, %d bytes, with %d lines, %d bytes",
			in.Path, countLines(existing), len(existing), countLines(in.Content), len(in.Content)))
	}
	if kind := documentKind(in.Path); kind != "" {
		// write_file refuses this, so the card should not imply it will work.
		lines = append(lines, warn("write_file writes plain text, so this %s would be refused", kind))
		return lines
	}
	return append(lines, body(in.Content, "added")...)
}

// previewEdit shows the match edit_file will make, or why it will refuse.
func previewEdit(in editArgs, workspace string) []Line {
	content, err := readForPreview(workspace, in.Path)
	if err != nil {
		return []Line{warn("%s cannot be read, so this edit will be refused", in.Path)}
	}
	m := resolveEdit(content, in.OldText, in.NewText)
	switch {
	case m.Count == 0:
		return []Line{warn("old_text is not in %s, so this edit will be refused", in.Path)}
	case m.Count > 1 && !in.ReplaceAll:
		return []Line{warn("old_text occurs %d times in %s, so this edit will be refused", m.Count, in.Path)}
	}

	where := fmt.Sprintf("edits %s at line %d", in.Path, m.Line)
	if in.ReplaceAll && m.Count > 1 {
		where = fmt.Sprintf("edits %s in %d places, the first at line %d", in.Path, m.Count, m.Line)
	}
	lines := []Line{plain("%s", where)}
	lines = append(lines, body(m.Old, "removed")...)
	return append(lines, body(m.New, "added")...)
}

// body renders text as at most previewLines lines of one kind, saying how much
// it left out.
func body(text, kind string) []Line {
	if text == "" {
		return nil
	}
	split := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// A trailing newline ends the last line rather than starting an empty one.
	if len(split) > 1 && split[len(split)-1] == "" {
		split = split[:len(split)-1]
	}
	lines := make([]Line, 0, previewLines+1)
	for i, l := range split {
		if i == previewLines {
			lines = append(lines, plain("… and %d more lines", len(split)-previewLines))
			break
		}
		lines = append(lines, Line{Kind: kind, Text: l})
	}
	return lines
}

// describeArgs is the fallback for MCP tools and anything without its own
// shape: the same JSON, one field per line, rather than one long blob.
func describeArgs(args []byte) []Line {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || len(fields) == 0 {
		if s := strings.TrimSpace(string(args)); s != "" && s != "{}" {
			return []Line{plain("%s", s)}
		}
		return nil
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	lines := make([]Line, 0, len(names))
	for _, n := range names {
		value := string(fields[n])
		// A JSON string reads better without its quotes and escapes.
		var s string
		if err := json.Unmarshal(fields[n], &s); err == nil {
			value = s
		}
		lines = append(lines, plain("%s: %s", n, oneLine(value)))
	}
	return lines
}

// readForPreview reads a workspace file through the same root the tools are
// confined to, refusing what edit_file would refuse.
func readForPreview(workspace, path string) (string, error) {
	root, err := sandbox{workspace}.open()
	if err != nil {
		return "", err
	}
	defer root.Close()

	info, err := root.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fs.ErrInvalid
	}
	if info.Size() > maxEditBytes {
		return "", fs.ErrInvalid
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return "", err
	}
	if isBinary(data) {
		return "", fs.ErrInvalid
	}
	return string(data), nil
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// oneLine keeps a long or multi-line value from breaking the card's layout.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
