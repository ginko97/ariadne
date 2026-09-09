package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

func TestCalcEvaluates(t *testing.T) {
	cases := []struct{ expr, want string }{
		{"240*0.15", "36"}, // exact rational arithmetic, not 35.999999999999996
		{"(2+3)*4", "20"},  // floatified but still prints as an integer
		{"7/2", "3.5"},     // the bug floatify exists to fix: Go alone gives 3
		{"10%3", "1"},      // % keeps integer semantics
		{"1/3*3", "1"},     // exact rationals, not 0.9999999999999999
		{"0x10+0", "16"},   // hex literal left alone, still evaluates
	}
	for _, tc := range cases {
		args, _ := json.Marshal(calcArgs{Expr: tc.expr})
		got, err := Calc{}.Call(context.Background(), args)
		if err != nil {
			t.Errorf("%s: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

// The model will send nonsense eventually. It must come back as an error the
// loop can turn into a tool_result, never a panic.
func TestCalcRejectsJunk(t *testing.T) {
	for _, expr := range []string{"os.Exit(1)", "1+", "hello", ""} {
		args, _ := json.Marshal(calcArgs{Expr: expr})
		if _, err := (Calc{}).Call(context.Background(), args); err == nil {
			t.Errorf("%q: expected an error, got none", expr)
		}
	}
}

func TestCalcRejectsBadJSON(t *testing.T) {
	if _, err := (Calc{}).Call(context.Background(), json.RawMessage(`{"expr":`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// stub is a second tool so Defs has something to order.
type stub struct{ name string }

func (s stub) Name() string            { return s.name }
func (s stub) Description() string     { return "stub " + s.name }
func (s stub) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stub) Call(context.Context, json.RawMessage) (string, error) {
	return s.name + " ran", nil
}

// Sorted output keeps the prompt byte-identical run to run.
func TestRegistryDefsAreSorted(t *testing.T) {
	r := New(stub{"zebra"}, Calc{}, stub{"alpha"})

	defs := r.Defs()
	if len(defs) != 3 {
		t.Fatalf("got %d defs, want 3", len(defs))
	}
	want := []string{"alpha", "calc", "zebra"}
	for i, w := range want {
		if defs[i].Name != w {
			t.Errorf("defs[%d] = %s, want %s", i, defs[i].Name, w)
		}
	}
	if !strings.Contains(defs[1].Description, "arithmetic") {
		t.Errorf("calc description not carried into ToolDef: %q", defs[1].Description)
	}
	if !json.Valid(defs[1].Schema) {
		t.Error("calc schema is not valid JSON")
	}
}

func TestRegistryDispatches(t *testing.T) {
	r := New(Calc{})

	got, err := r.Call(context.Background(), llm.ToolCall{
		Name: "calc",
		Args: json.RawMessage(`{"expr":"6*7"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "42" {
		t.Errorf("got %s, want 42", got)
	}
}

// An unknown name is recoverable: the loop turns it into an IsError block and
// the model can choose a tool that exists.
func TestRegistryUnknownTool(t *testing.T) {
	r := New(Calc{})

	_, err := r.Call(context.Background(), llm.ToolCall{Name: "rm_rf", Args: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("got %v, want ErrUnknownTool", err)
	}
}
