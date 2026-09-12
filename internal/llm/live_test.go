//go:build live

package llm

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/ginko97/ariadne/internal/dotenv"
)

// Run with:
//
//	go test ./internal/llm/ -tags live -run TestLive -v
//
// Override the model without editing this file:
//
//	ARIADNE_MODEL=gemini-3-flash go test ./internal/llm/ -tags live -run TestLive -v
//
// List the model IDs your key can actually reach:
//
//	curl https://generativelanguage.googleapis.com/v1beta/openai/models \
//	  -H "Authorization: Bearer $GEMINI_API_KEY"
func TestLiveToolCall(t *testing.T) {
	_ = dotenv.Load()

	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("set GEMINI_API_KEY or add it to .env")
	}

	model := os.Getenv("ARIADNE_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}

	o := NewOpenAI(key,
		WithBaseURL("https://generativelanguage.googleapis.com/v1beta/openai"),
	)

	resp, err := o.Complete(context.Background(), Request{
		Model: model,
		Messages: []Message{{
			Role:   RoleUser,
			Blocks: []Block{{Type: BlockText, Text: "What is 15% of 240? Use the calc tool."}},
		}},
		Tools: []ToolDef{{
			Name:        "calc",
			Description: "Evaluate an arithmetic expression",
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{"expr":{"type":"string"}},
				"required":["expr"]
			}`),
		}},
	})
	if err != nil {
		t.Fatalf("live call failed: %v", err)
	}

	// Read these before trusting the assertion below. First contact is where you
	// find out whether finish_reason arrives as documented, whether usage is
	// populated, and whether this model fires tools at all.
	t.Logf("model=%s stop=%s usage=%+v", model, resp.Stop, resp.Usage)
	for i, b := range resp.Blocks {
		t.Logf("block[%d] type=%s text=%q name=%s args=%s", i, b.Type, b.Text, b.Name, b.Args)
	}

	if resp.Stop != StopToolUse {
		t.Errorf("model did not request a tool — stop=%s", resp.Stop)
	}
}
