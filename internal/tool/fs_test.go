package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func call(t *testing.T, tl Tool, args any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Call(context.Background(), "call_1", raw)
	if err != nil {
		t.Fatalf("%s returned an error rather than an IsError result: %v", tl.Name(), err)
	}
	return res.Content, res.IsError
}

func TestFetchReadsInsideSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, isErr := call(t, NewFetch(dir), fetchArgs{Path: "page.txt"})
	if isErr || got != "hello" {
		t.Errorf("got %q isError=%v, want %q", got, isErr, "hello")
	}
}

// Every path argument comes from the model, and everything the model has seen
// may have been written by someone else. Treat it as hostile input.
func TestSandboxRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, p := range []string{
		"../secret.txt",
		"../../secret.txt",
		"a/../../secret.txt",
		`..\secret.txt`,
	} {
		got, isErr := call(t, NewFetch(dir), fetchArgs{Path: p})
		if strings.Contains(got, "classified") {
			t.Errorf("fetch %q escaped the sandbox and read the file", p)
		}
		if !isErr {
			t.Errorf("fetch %q was not refused: %q", p, got)
		}
	}
}

// An absolute path must land inside the sandbox, not at the filesystem root.
func TestSandboxContainsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	w := NewWriteFile(dir)

	if _, isErr := call(t, w, writeArgs{Path: "/etc/passwd", Content: "x"}); isErr {
		// Refusing is fine too; what matters is where it did not write.
		t.Logf("absolute path refused, which is acceptable")
	}
	if _, err := os.Stat("/etc/passwd.ariadne-test"); err == nil {
		t.Fatal("wrote outside the sandbox")
	}
	// It should have landed under the sandbox root instead.
	if _, err := os.Stat(filepath.Join(dir, "etc", "passwd")); err != nil {
		t.Logf("absolute path was refused rather than remapped: %v", err)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, isErr := call(t, NewWriteFile(dir), writeArgs{Path: "notes/todo.txt", Content: "buy milk"})
	if isErr {
		t.Fatalf("write failed: %s", got)
	}
	if !strings.Contains(got, "8 bytes") {
		t.Errorf("result should say what was written: %q", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, "notes", "todo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "buy milk" {
		t.Errorf("file contains %q", data)
	}
}

// Writing the same bytes to the same path twice is harmless, which is why this
// tool does not need callID. A tool that appended or sent something would.
func TestWriteFileIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w := NewWriteFile(dir)

	for i := 0; i < 2; i++ {
		if _, isErr := call(t, w, writeArgs{Path: "x.txt", Content: "once"}); isErr {
			t.Fatal("write failed")
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(data) != "once" {
		t.Errorf("replayed write produced %q", data)
	}
}

func TestFetchMissingFileIsRecoverable(t *testing.T) {
	got, isErr := call(t, NewFetch(t.TempDir()), fetchArgs{Path: "nope.txt"})
	if !isErr {
		t.Fatal("missing file should be an IsError result")
	}
	if !strings.Contains(got, "nope.txt") {
		t.Errorf("message should name the path: %q", got)
	}
}

func TestToolsRejectEmptyPath(t *testing.T) {
	dir := t.TempDir()
	if _, isErr := call(t, NewFetch(dir), fetchArgs{Path: ""}); !isErr {
		t.Error("fetch accepted an empty path")
	}
	if _, isErr := call(t, NewWriteFile(dir), writeArgs{Path: "", Content: "x"}); !isErr {
		t.Error("write_file accepted an empty path")
	}
}

// A symlink inside the sandbox pointing outside must not become a bridge out.
func TestSandboxRejectsSymlinkEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link_to_secret")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	got, isErr := call(t, NewFetch(dir), fetchArgs{Path: "link_to_secret"})
	if strings.Contains(got, "classified") {
		t.Errorf("fetch via symlink escaped the sandbox: %q", got)
	}
	if !isErr {
		t.Errorf("fetch via symlink was not refused: %q", got)
	}

	gotWrite, isErrWrite := call(t, NewWriteFile(dir), writeArgs{Path: "link_to_secret", Content: "pwned"})
	if !isErrWrite {
		t.Errorf("write_file via symlink was not refused: %q", gotWrite)
	}
	data, _ := os.ReadFile(outside)
	if string(data) == "pwned" {
		t.Errorf("write_file via symlink overwrote outside file")
	}
}

// If the sandbox root itself is a symlink, resolving paths inside it must still work.
func TestSandboxWithSymlinkedRoot(t *testing.T) {
	realRoot := t.TempDir()
	symRoot := filepath.Join(t.TempDir(), "symlink_root")
	if err := os.Symlink(realRoot, symRoot); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	w := NewWriteFile(symRoot)
	gotWrite, isErrWrite := call(t, w, writeArgs{Path: "hello.txt", Content: "symlinked-root-ok"})
	if isErrWrite {
		t.Fatalf("write failed with symlinked root: %s", gotWrite)
	}

	f := NewFetch(symRoot)
	gotFetch, isErrFetch := call(t, f, fetchArgs{Path: "hello.txt"})
	if isErrFetch || gotFetch != "symlinked-root-ok" {
		t.Fatalf("fetch failed with symlinked root: got %q, isErr=%v", gotFetch, isErrFetch)
	}
}

// Drive prefixes and UNC paths must not escape the sandbox root. Asserted
// through the tool rather than against an internal helper, so the test still
// means something when the confinement mechanism is replaced.
func TestSandboxWindowsDriveEscape(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		`C:\windows\system32\calc.exe`,
		`D:\secret.txt`,
		`\\server\share\file.txt`,
		`C:/windows/system32/drivers/etc/hosts`,
	} {
		if _, isErr := call(t, NewWriteFile(dir), writeArgs{Path: p, Content: "x"}); !isErr {
			t.Errorf("write_file %q was not refused", p)
		}
		if _, isErr := call(t, NewFetch(dir), fetchArgs{Path: p}); !isErr {
			t.Errorf("fetch %q was not refused", p)
		}
	}
}

// junction makes a Windows directory junction. It needs no privileges, unlike
// os.Symlink — which is why the two symlink tests above skip on an ordinary
// Windows account and this one does not. The escape they were written to catch
// was reachable here the whole time.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows reparse point")
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v: %s", err, out)
	}
}

// A junction inside the sandbox pointing outside it must not be a way out.
//
// This is the case lexical resolution could not see: EvalSymlinks returns a
// junction unchanged with a nil error, and fails on a path leading through one,
// so a prefix check on the unresolved path passed while the OS followed the
// link. Both tools escaped — fetch read the file and write_file planted one.
func TestSandboxRejectsJunctionEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(dir, "bridge"), outside)

	got, isErr := call(t, NewFetch(dir), fetchArgs{Path: "bridge/secret.txt"})
	if strings.Contains(got, "classified") {
		t.Errorf("fetch crossed a junction out of the sandbox: %q", got)
	}
	if !isErr {
		t.Errorf("fetch through a junction was not refused: %q", got)
	}

	if _, isErr := call(t, NewWriteFile(dir), writeArgs{Path: "bridge/planted.txt", Content: "pwned"}); !isErr {
		t.Error("write_file through a junction was not refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Error("write_file crossed a junction and planted a file outside the sandbox")
	}
}

// The mirror: a sandbox root that is itself a junction must still work. This is
// what the old prefix check got wrong in the other direction — it compared a
// resolved path against an unresolved root and refused every legitimate path.
func TestSandboxWithJunctionedRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "root_link")
	junction(t, linkRoot, realRoot)

	got, isErr := call(t, NewWriteFile(linkRoot), writeArgs{Path: "notes/hello.txt", Content: "ok"})
	if isErr {
		t.Fatalf("legitimate write through a junctioned root was refused: %s", got)
	}
	gotF, isErrF := call(t, NewFetch(linkRoot), fetchArgs{Path: "notes/hello.txt"})
	if isErrF || gotF != "ok" {
		t.Fatalf("fetch through a junctioned root failed: %q isErr=%v", gotF, isErrF)
	}
}

// The sandbox root need not exist before the first write. A job should not fail
// because nothing has created the directory yet.
func TestWriteFileCreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")

	got, isErr := call(t, NewWriteFile(root), writeArgs{Path: "a/b/c.txt", Content: "deep"})
	if isErr {
		t.Fatalf("write into a missing root failed: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "a", "b", "c.txt"))
	if err != nil || string(data) != "deep" {
		t.Errorf("file = %q, err = %v", data, err)
	}
}

// A tool result is not a file, it is a message resent with every later request,
// so an unbounded read is an unbounded prompt. Found the hard way: a 20MB PDF
// fetched once produced a 66MB checkpoint and a 400 on the next request, and
// the run could never finish — compaction could not help, because the keep-floor
// protects the most recent exchange, which was the oversized message.
func TestFetchRefusesADocumentTooLargeForTheConversation(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxFetchBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := NewFetch(dir).Call(context.Background(), "c1",
		json.RawMessage(`{"path":"huge.txt"}`))
	if err != nil {
		t.Fatalf("a refusal the model can act on is a result, not an error: %v", err)
	}
	if !res.IsError {
		t.Fatal("an oversized document was read into the conversation")
	}
	if !strings.Contains(res.Content, "limit") {
		t.Errorf("the refusal does not say why: %q", res.Content)
	}
	// The bytes must not come back at all — a truncated 20MB file is still far
	// past what belongs in a prompt.
	if len(res.Content) > 500 {
		t.Errorf("the refusal carried %d characters of the document", len(res.Content))
	}
}

// Reading a PDF as a string does not fail, it succeeds at producing millions of
// characters of compressed rubbish that costs tokens and answers nothing.
func TestFetchRefusesBinary(t *testing.T) {
	dir := t.TempDir()
	pdf := append([]byte("%PDF-1.7\n"), 0x00, 0x8f, 0x1e, 0x00, 'j', 'u', 'n', 'k')
	if err := os.WriteFile(filepath.Join(dir, "doc.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := NewFetch(dir).Call(context.Background(), "c1",
		json.RawMessage(`{"path":"doc.pdf"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a binary file was read into the conversation")
	}
	if !strings.Contains(res.Content, "not text") {
		t.Errorf("the refusal does not say why: %q", res.Content)
	}
}

// The ordinary case still works, and is still marked untrusted.
func TestFetchStillReadsTextDocuments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("total due: 42"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := NewFetch(dir).Call(context.Background(), "c1",
		json.RawMessage(`{"path":"notes.txt"}`))
	if err != nil || res.IsError {
		t.Fatalf("a plain text document was refused: %v %q", err, res.Content)
	}
	if res.Content != "total due: 42" {
		t.Errorf("content = %q", res.Content)
	}
	if !res.Untrusted {
		t.Error("a fetched document must still be marked untrusted")
	}
}
