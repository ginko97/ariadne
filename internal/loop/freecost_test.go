package loop

import (
	"context"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// A model that costs nothing is a known cost of zero, not an unknown one, so
// -max-cost lets it run: OpenRouter's free models, which report cost 0, and a
// model on this machine, priced Free.
func TestFreeModelIsNotAnUnknownCost(t *testing.T) {
	reportedZero := llm.Usage{InputTokens: 2_000_000, OutputTokens: 500_000, CostReported: true}
	unreported := llm.Usage{InputTokens: 2_000_000, OutputTokens: 500_000}

	for _, tc := range []struct {
		name  string
		usage llm.Usage
		price Price
	}{
		{"provider reports cost 0", reportedZero, Price{}},
		{"local model, priced free", unreported, Price{Free: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := func(r llm.Response) llm.Response { r.Usage = tc.usage; return r }
			price := tc.price
			a := &Agent{
				Provider: &llm.Fake{Responses: []llm.Response{
					step(toolUseResponse("c1", "calc", `{}`, 0, 0)),
					step(endResponse("done", 0, 0)),
				}},
				Model: "free", MaxSteps: 10, MaxCost: 0.01,
				PriceResolver: func(string) Price { return price },
				RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
					return llm.ToolResult{Content: "1"}, nil
				},
			}
			s := NewState("r", "go")
			if _, err := a.Run(context.Background(), s); err != nil {
				t.Fatalf("a free model was stopped: %v", err)
			}
			if s.Steps != 2 || s.Cost != 0 || s.UnpricedSteps != 0 {
				t.Errorf("steps=%d cost=%v unpriced=%d, want 2, 0, 0", s.Steps, s.Cost, s.UnpricedSteps)
			}
		})
	}
}
