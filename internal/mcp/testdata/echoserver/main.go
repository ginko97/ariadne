// Command echoserver is a minimal MCP server used by the tests in internal/mcp.
//
// It exists so the adapter can be tested against the real protocol over a real
// pipe, with no Node, no network, and no API key. The tests spawn it with
// `go run ./testdata/echoserver`.
//
// It lives under testdata/ so the go tool does not build it as part of ./...
package main

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

type upperArgs struct {
	Text string `json:"text" jsonschema:"the text to upper-case"`
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "0.0.1"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "Return the text you were given",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: in.Text}},
		}, nil, nil
	})

	// Deliberately fails on empty input, so the tests can prove IsError survives
	// the round trip distinctly from a transport failure.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "upper",
		Description: "Upper-case the text you were given",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in upperArgs) (*mcp.CallToolResult, any, error) {
		if in.Text == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "upper: text is required"}},
				IsError: true,
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: strings.ToUpper(in.Text)}},
		}, nil, nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
