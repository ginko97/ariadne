package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

func preview(t *testing.T, name string, args any, workspace string) []Line {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Preview(name, b, workspace)
}

// text joins a preview for matching, marking each line the way the card does.
func text(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		switch l.Kind {
		case "added":
			b.WriteString("+ ")
		case "removed":
			b.WriteString("- ")
		case "warn":
			b.WriteString("! ")
		}
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func wants(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("preview is missing %q:\n%s", w, got)
		}
	}
}

// The card has to distinguish a new file from one being overwritten, because
// that is the whole difference between "sure" and "wait".
func TestPreviewSaysWhatWriteFileReplaces(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "notes.txt", "one\ntwo\nthree\n")

	got := text(preview(t, WriteFileName, writeArgs{Path: "notes.txt", Content: "one\n"}, dir))
	wants(t, got, "replaces notes.txt", "3 lines", "14 bytes", "+ one")

	got = text(preview(t, WriteFileName, writeArgs{Path: "new.txt", Content: "hello\n"}, dir))
	wants(t, got, "creates new.txt", "+ hello")
	if strings.Contains(got, "replaces") {
		t.Errorf("a file that does not exist is not replaced:\n%s", got)
	}

	// Long content is cut, and says so rather than running off the card.
	long := strings.Repeat("line\n", 100)
	got = text(preview(t, WriteFileName, writeArgs{Path: "long.txt", Content: long}, dir))
	wants(t, got, "and 80 more lines")
	if n := strings.Count(got, "+ line"); n != previewLines {
		t.Errorf("showed %d body lines, want %d:\n%s", n, previewLines, got)
	}
}

// The edit card names the line it lands on and shows both sides. Without the
// line number, "which of the four identical-looking places is this" has no
// answer.
func TestPreviewShowsTheEditWithItsLine(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "notes.txt", "Project notes\nOwner: Ginko\nStatus: draft\n")

	got := text(preview(t, EditFileName, editArgs{
		Path: "notes.txt", OldText: "Status: draft", NewText: "Status: final",
	}, dir))
	wants(t, got, "edits notes.txt at line 3", "- Status: draft", "+ Status: final")
}

// The preview and the tool have to agree, or the card describes an edit that
// then fails. The case that caught this in the wild: a CRLF file and a model
// writing "\n".
func TestPreviewMatchesWhatEditFileWillDo(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "crlf.txt", "Project notes\r\nOwner: Ginko\r\nStatus: draft\r\n")

	args := editArgs{Path: "crlf.txt", OldText: "Owner: Ginko\n", NewText: "Owner: Ginko (owner)\n"}
	got := text(preview(t, EditFileName, args, dir))
	wants(t, got, "edits crlf.txt at line 2", "- Owner: Ginko", "+ Owner: Ginko (owner)")
	if strings.Contains(got, "refused") {
		t.Fatalf("preview refuses an edit the tool accepts:\n%s", got)
	}

	// And the tool does accept it.
	if out, isErr := editIn(t, dir, args); isErr {
		t.Fatalf("edit_file refused what the preview promised: %s", out)
	}
	if body := readTemp(t, dir+"/crlf.txt"); !strings.Contains(body, "Owner: Ginko (owner)\r\n") {
		t.Errorf("file did not get the previewed edit: %q", body)
	}
}

// A call that cannot succeed says so on the card. Approving something that is
// about to be refused teaches the operator that the buttons do not matter.
func TestPreviewWarnsWhenTheEditWillBeRefused(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "notes.txt", "a\nb\na\n")

	got := text(preview(t, EditFileName, editArgs{Path: "notes.txt", OldText: "zzz", NewText: "x"}, dir))
	wants(t, got, "! old_text is not in notes.txt")

	got = text(preview(t, EditFileName, editArgs{Path: "notes.txt", OldText: "a\n", NewText: "x\n"}, dir))
	wants(t, got, "! old_text occurs 2 times")

	got = text(preview(t, EditFileName, editArgs{Path: "missing.txt", OldText: "a", NewText: "b"}, dir))
	wants(t, got, "! missing.txt cannot be read")

	// write_file refuses to write a document, so the card must not imply it
	// will work.
	got = text(preview(t, WriteFileName, writeArgs{Path: "report.pdf", Content: "hello"}, dir))
	wants(t, got, "! write_file writes plain text")
}

// The URL in full, the argv one line each, the note in words: the three cards
// where a JSON blob hid what mattered.
func TestPreviewReadsTheOtherGatedTools(t *testing.T) {
	dir := t.TempDir()

	got := text(preview(t, WebFetchName, map[string]string{"url": "https://example.com/a?b=c"}, dir))
	wants(t, got, "GET https://example.com/a?b=c", "host: example.com")

	got = text(preview(t, "exec", execArgs{Argv: []string{"go", "test", "./..."}}, dir))
	wants(t, got, "run go", "test", "./...", "in "+dir, "! exec is not confined")

	got = text(preview(t, "remember", rememberArgs{Note: "Ginko prefers tabs"}, dir))
	wants(t, got, "remember: Ginko prefers tabs")
	if strings.Contains(got, "{") {
		t.Errorf("the note is shown as JSON:\n%s", got)
	}
}

// An MCP tool has no shape this package knows, so the fallback still has to be
// readable: one field per line, strings unquoted.
func TestPreviewFallsBackToOneFieldPerLine(t *testing.T) {
	got := text(Preview("fs__move_file", json.RawMessage(`{"source":"a.txt","destination":"b.txt"}`), ""))
	wants(t, got, "destination: b.txt\n", "source: a.txt\n")
	if strings.Contains(got, `"`) {
		t.Errorf("JSON quoting survived into the card:\n%s", got)
	}

	if lines := Preview("whatever", json.RawMessage(`{}`), ""); len(lines) != 0 {
		t.Errorf("empty arguments should draw no preview lines, got %v", lines)
	}
}
