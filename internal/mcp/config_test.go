package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/tool"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The file shape is the one the ecosystem already uses, so a config written for
// another MCP client works here unchanged. That is the whole reason not to
// invent one.
func TestLoadConfigReadsTheEcosystemShape(t *testing.T) {
	p := writeConfig(t, `{
  "mcpServers": {
    "filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/srv/docs"]},
    "git":        {"command": "uvx", "args": ["mcp-server-git"], "disabled": true}
  }
}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(cfg.Servers))
	}
	fs := cfg.Servers["filesystem"]
	if fs.Command != "npx" || len(fs.Args) != 3 {
		t.Errorf("filesystem = %+v", fs)
	}
	if !cfg.Servers["git"].Disabled {
		t.Error("disabled was not read; an entry kept in the file would start anyway")
	}
}

// A server with no command is a config error, not something to discover when
// exec fails with an empty argv.
func TestLoadConfigRejectsAServerWithNoCommand(t *testing.T) {
	p := writeConfig(t, `{"mcpServers": {"broken": {"args": ["x"]}}}`)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("a server with no command was accepted")
	}
}

func TestLoadConfigReportsTheFileOnBadJSON(t *testing.T) {
	p := writeConfig(t, `{"mcpServers": {`)
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	// The path, because a config error names a file somebody has to open.
	if !strings.Contains(err.Error(), "mcp.json") {
		t.Errorf("error does not name the file: %v", err)
	}
}

// fakeTool is a stand-in; only its name matters to collision checking.
type fakeTool string

func (f fakeTool) Name() string          { return string(f) }
func (fakeTool) Description() string     { return "" }
func (fakeTool) Schema() json.RawMessage { return nil }
func (fakeTool) Call(context.Context, string, json.RawMessage) (llm.ToolResult, error) {
	panic("collision checking never calls a tool")
}

// tool.New keeps the last tool of a given name and says nothing, so a server
// offering "fetch" would replace the one confined by os.Root with a remote one
// answering to somebody else's process. That is a silent privilege change and
// the operator has to resolve it, not the map-insertion order.
func TestCheckCollisionsRefusesShadowingALocalTool(t *testing.T) {
	local := []tool.Tool{tool.Calc{}, tool.NewFetch("workspace")}
	remote := []tool.Tool{fakeTool("fetch")}

	err := CheckCollisions(local, remote)
	if err == nil {
		t.Fatal("a remote tool silently replaced a sandboxed built-in")
	}
	if !strings.Contains(err.Error(), "fetch") {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

// Two servers offering the same name is the same defect between strangers:
// whichever is dialled last wins, and nothing says so.
func TestCheckCollisionsRefusesTwoServersOfferingOneName(t *testing.T) {
	remote := []tool.Tool{fakeTool("search"), fakeTool("search")}
	if err := CheckCollisions(nil, remote); err == nil {
		t.Fatal("two servers were allowed to offer the same tool name")
	}
}

func TestCheckCollisionsAllowsDistinctNames(t *testing.T) {
	local := []tool.Tool{tool.Calc{}, tool.NewFetch("workspace")}
	remote := []tool.Tool{fakeTool("read_file"), fakeTool("git_log")}
	if err := CheckCollisions(local, remote); err != nil {
		t.Errorf("distinct names were refused: %v", err)
	}
}
