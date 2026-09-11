package tool_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/testenv"
	"github.com/ginko97/ariadne/internal/tool"
)

func TestLiveAgentRun(t *testing.T) {
	_ = testenv.Load()

	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("set GEMINI_API_KEY or add it to .env")
	}
	model := os.Getenv("ARIADNE_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}

	reg := tool.New(tool.Calc{})
	o := llm.NewOpenAI(key,
		llm.WithBaseURL("https://generativelanguage.googleapis.com/v1beta/openai"))

	a := &loop.Agent{
		Provider: o,
		Model:    model,
		Tools:    reg.Defs(),
		RunTool:  reg.Call,
		MaxSteps: 5,
	}

	s := loop.NewState("live-1",
		"What is 15% of 240? Use the calc tool, then state the answer.")

	answer, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("run failed after %d steps: %v", s.Steps, err)
	}

	t.Logf("steps=%d cost=%v", s.Steps, s.Cost)
	t.Logf("answer=%q", answer)
	for i, m := range s.Messages {
		for j, b := range m.Blocks {
			t.Logf("msg[%d].block[%d] role=%s type=%s text=%q name=%s args=%s content=%q",
				i, j, m.Role, b.Type, b.Text, b.Name, b.Args, b.Content)
		}
	}

	if !strings.Contains(answer, "36") {
		t.Errorf("answer does not contain 36: %q", answer)
	}
}
