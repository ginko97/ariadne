package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// ListFilesName is the tool's name.
const ListFilesName = "list_files"

const (
	// maxListEntries is how many entries one result shows. A folder of
	// thousands of files is not something to paste into a conversation; the
	// count of the rest is, so the model knows to look closer.
	maxListEntries = 500
	// maxListRead bounds how many entries are read to sort, so a folder with
	// a million files costs a bounded amount of memory.
	maxListRead = 10000
)

// ListFiles lists a folder inside the workspace, in the same sandbox as fetch.
//
// Without it a model can read any file whose exact name it is told, and no
// other: asked to "list the files in the folder", it could only say it had no
// way to, and a person had to paste a 150-character file name before the file
// could be read.
//
// The result is untrusted. A file name is text somebody else chose — a
// downloaded file can be named anything, including an instruction — so it is
// fenced like a document's contents. Control characters in names are replaced,
// so a name cannot forge a line of the listing.
type ListFiles struct{ sandbox }

func NewListFiles(root string) ListFiles { return ListFiles{sandbox{root}} }

var _ Tool = ListFiles{}

func (ListFiles) Name() string { return ListFilesName }

func (ListFiles) Description() string {
	return "List the files and folders in a folder of the workspace, with each file's " +
		"size and last-modified time. path is relative to the workspace; leave it empty " +
		"for the workspace itself. Folders end with /. Use it to find a file's exact " +
		"name before fetching it. This tool cannot list outside the workspace."
}

func (ListFiles) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "folder to list, relative to the workspace; empty for the workspace itself"}
  },
  "additionalProperties": false
}`)
}

type listArgs struct {
	Path string `json:"path"`
}

func (l ListFiles) Call(_ context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}
	var in listArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return fail("list_files: bad arguments: %v", err)
		}
	}
	dir := strings.TrimSpace(in.Path)
	if dir == "" {
		dir = "."
	}

	root, err := l.open()
	if err != nil {
		return fail("list_files: %v", err)
	}
	defer root.Close()
	f, err := root.Open(dir)
	if err != nil {
		return fail("list_files: cannot open %q: %v", dir, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return fail("list_files: %v", err)
	} else if !st.IsDir() {
		return fail("list_files: %q is a file, not a folder; fetch reads files", dir)
	}

	entries, err := f.ReadDir(maxListRead)
	if err != nil && !errors.Is(err, io.EOF) {
		return fail("list_files: cannot list %q: %v", dir, err)
	}
	more := false
	if len(entries) == maxListRead {
		if extra, _ := f.ReadDir(1); len(extra) > 0 {
			more = true
		}
	}
	// Folders first, then files, each by name: the order a person scans in.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var b strings.Builder
	shown := entries
	if len(shown) > maxListEntries {
		shown = shown[:maxListEntries]
	}
	switch {
	case len(entries) == 0:
		fmt.Fprintf(&b, "%s is empty.\n", dir)
	case more:
		fmt.Fprintf(&b, "%s holds more than %d entries; the first %d:\n", dir, maxListRead, len(shown))
	default:
		fmt.Fprintf(&b, "%s holds %d entries:\n", dir, len(entries))
	}
	for _, e := range shown {
		b.WriteString(listLine(e))
		b.WriteByte('\n')
	}
	if n := len(entries) - len(shown); n > 0 {
		fmt.Fprintf(&b, "… and %d more not shown. List a subfolder, or fetch a file by name.\n", n)
	}
	return llm.ToolResult{Content: strings.TrimRight(b.String(), "\n"), Untrusted: true}, nil
}

// listLine is one entry: name, and for a file its size and modified time.
// Links are shown as links and not followed; whether one leads outside the
// workspace is for os.Root to decide when it is opened.
func listLine(e fs.DirEntry) string {
	name := safeName(e.Name())
	switch {
	case e.IsDir():
		return name + "/"
	case e.Type()&fs.ModeSymlink != 0:
		return name + "  (link)"
	}
	info, err := e.Info()
	if err != nil {
		return name
	}
	return fmt.Sprintf("%s  %s  %s", name, humanSize(info.Size()), info.ModTime().Format("2006-01-02 15:04"))
}

// safeName replaces control characters, so a name with a newline in it cannot
// pass for two entries.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

func humanSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}
