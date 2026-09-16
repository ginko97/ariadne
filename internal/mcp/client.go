// Package mcp adapts the tools of an MCP server to the tool.Tool interface, so
// the registry and the loop cannot tell a remote tool from a local one.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/tool"
)

const (
	clientName    = "ariadne"
	clientVersion = "0.0.1"
)

// Server is a connection to one MCP server running as a subprocess over stdio.
type Server struct {
	session *mcpsdk.ClientSession
}

// Dial starts command as a subprocess and completes the MCP handshake.
//
// The protocol churn in revision 2026-07-28 — initialize removed, server/discover
// added, resultType required on every result — lives inside the SDK. None of it
// leaks into this file, which is the reason for taking the dependency rather than
// speaking JSON-RPC by hand.
func Dial(ctx context.Context, command string, args ...string) (*Server, error) {
	c := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil)

	t := &mcpsdk.CommandTransport{Command: exec.CommandContext(ctx, command, args...)}

	sess, err := c.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %q: %w", command, err)
	}
	return &Server{session: sess}, nil
}

// Close ends the session and stops the subprocess.
func (s *Server) Close() error { return s.session.Close() }

// Tools lists what the server offers, each wrapped so tool.Registry accepts it.
//
// Call it once at startup: the schemas are marshalled here rather than per
// request, and the result is what gets handed to the model as ToolDefs.
func (s *Server) Tools(ctx context.Context) ([]tool.Tool, error) {
	res, err := s.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}

	out := make([]tool.Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		// InputSchema is `any` — whatever the server sent, as long as it marshals
		// to valid JSON Schema. Flatten it to bytes once, here.
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: tool %q has an unmarshalable schema: %w", t.Name, err)
		}
		out = append(out, &remoteTool{
			session:     s.session,
			name:        t.Name,
			description: t.Description,
			schema:      schema,
		})
	}
	return out, nil
}

// remoteTool forwards one named tool to the MCP session.
type remoteTool struct {
	session     *mcpsdk.ClientSession
	name        string
	description string
	schema      json.RawMessage
}

var _ tool.Tool = (*remoteTool)(nil)

func (r *remoteTool) Name() string            { return r.name }
func (r *remoteTool) Description() string     { return r.description }
func (r *remoteTool) Schema() json.RawMessage { return r.schema }

// Call forwards the model's arguments to the server.
//
// callID is unused: MCP has no per-call idempotency field, so a duplicate call
// cannot be suppressed at this layer. Deduplication has to happen in the loop,
// by recording which callIDs completed before a crash. The parameter stays named
// so that stops being a surprise.
func (r *remoteTool) Call(ctx context.Context, callID string, args json.RawMessage) (llm.ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	// Arguments is `any` that must marshal to JSON, so raw JSON passes through
	// untouched — no map[string]any round trip turning ints into float64s.
	res, err := r.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      r.name,
		Arguments: args,
	})
	if err != nil {
		// Transport or protocol failure: the tool could not be reached at all,
		// which is what a returned error means in the Tool contract.
		return llm.ToolResult{}, fmt.Errorf("mcp: call %q: %w", r.name, err)
	}

	// IsError means the tool ran and reported failure — recoverable, the model
	// sees it. That is what ToolResult.IsError exists to express.
	//
	// Untrusted always. An MCP server is somebody else's code, and what it
	// returns is usually somebody else's content besides — a file, a page, an
	// issue body. Leaving this unset meant the fence the system prompt tells the
	// model to look for was never drawn around any of it: a filesystem server
	// reading invoice-2291.html delivered the injection bare, where fetch
	// reading the same file fences it. Per-server trust would be a config
	// option, and nothing yet has earned one.
	return llm.ToolResult{
		Content:   flattenText(res.Content),
		IsError:   res.IsError,
		Untrusted: true,
	}, nil
}

// flattenText keeps the text parts of an MCP result and drops everything else.
//
// CallToolResult.Content is []Content — TextContent, ImageContent, AudioContent,
// ResourceLink, EmbeddedResource. llm.ToolResult.Content is a string, because
// Block.Content is a string on the wire regardless. Non-text results are lost
// here; that is a deliberate limitation of the Tool contract, not an oversight.
func flattenText(content []mcpsdk.Content) string {
	var parts []string
	for _, c := range content {
		if tc, ok := c.(*mcpsdk.TextContent); ok && tc.Text != "" {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
