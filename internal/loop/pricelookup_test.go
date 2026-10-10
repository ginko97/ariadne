package loop

import (
	"context"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

type ctxKey struct{}

// The price is looked up only for a step the provider did not price itself,
// and with the turn's context, so a slow lookup ends when the turn does.
func TestPriceLookedUpOnlyWhenCostUnreported(t *testing.T) {
	for name, tc := range map[string]struct {
		usage     llm.Usage
		wantCalls int
	}{
		"provider reported the cost": {llm.Usage{InputTokens: 10, OutputTokens: 5, Cost: 0.002, CostReported: true}, 0},
		"provider reported nothing":  {llm.Usage{InputTokens: 10, OutputTokens: 5}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			resp := endResponse("done", 0, 0)
			resp.Usage = tc.usage
			calls, sawTurnCtx := 0, false
			a := &Agent{
				Provider: &llm.Fake{Responses: []llm.Response{resp}}, Model: "m", MaxSteps: 2,
				PriceResolver: func(ctx context.Context, _ string) Price {
					calls++
					sawTurnCtx = ctx.Value(ctxKey{}) == "turn"
					return Price{InputPerMTok: 1, OutputPerMTok: 1}
				},
			}
			ctx := context.WithValue(context.Background(), ctxKey{}, "turn")
			if _, err := a.Run(ctx, NewState("r", "hi")); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Errorf("PriceResolver called %d times, want %d", calls, tc.wantCalls)
			}
			if calls > 0 && !sawTurnCtx {
				t.Error("PriceResolver was not given the turn's context")
			}
		})
	}
}
