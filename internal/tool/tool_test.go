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
		got, err := Calc{}.Call(context.Background(), "call_1", args)
		if err != nil {
			t.Errorf("%s: unreachable-tool error: %v", tc.expr, err)
			continue
		}
		if got.IsError {
			t.Errorf("%s: reported failure: %s", tc.expr, got.Content)
			continue
		}
		if got.Content != tc.want {
			t.Errorf("%s = %s, want %s", tc.expr, got.Content, tc.want)
		}
	}
}

// The model will send nonsense eventually. Every one of these is something the
// model can fix, so they come back as IsError results — not as a returned error,
// which is reserved for "the tool could not be reached at all".
func TestCalcRejectsJunk(t *testing.T) {
	for _, expr := range []string{"os.Exit(1)", "1+", "hello", ""} {
		args, _ := json.Marshal(calcArgs{Expr: expr})
		res, err := (Calc{}).Call(context.Background(), "call_1", args)
		if err != nil {
			t.Errorf("%q: want IsError result, got a returned error: %v", expr, err)
			continue
		}
		if !res.IsError {
			t.Errorf("%q: expected IsError, got %q", expr, res.Content)
		}
	}
}

func TestCalcRejectsBadJSON(t *testing.T) {
	res, err := (Calc{}).Call(context.Background(), "call_1", json.RawMessage(`{"expr":`))
	if err != nil {
		t.Fatalf("want IsError result, got a returned error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for malformed JSON, got %q", res.Content)
	}
}

// stub is a second tool so Defs has something to order.
type stub struct{ name string }

func (s stub) Name() string            { return s.name }
func (s stub) Description() string     { return "stub " + s.name }
func (s stub) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stub) Call(context.Context, string, json.RawMessage) (llm.ToolResult, error) {
	return llm.ToolResult{Content: s.name + " ran"}, nil
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
	if got.Content != "42" {
		t.Errorf("got %s, want 42", got.Content)
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

// Go's bitwise operators mean something no one asking a calculator wants, and
// they produce a number rather than an error. "2^3 % 5" returned 1 instead of 3
// — silently, because floatify (which would have made ^ a type error) is
// skipped whenever % is present.
func TestCalcRefusesBitwiseOperators(t *testing.T) {
	for _, expr := range []string{"2^3", "2^3 % 5", "10 % 3 + 2^3", "8 >> 1", "6 & 3", "6 | 3"} {
		args, _ := json.Marshal(calcArgs{Expr: expr})
		res, err := (Calc{}).Call(context.Background(), "c1", args)
		if err != nil {
			t.Errorf("%q: want an IsError result, got a returned error: %v", expr, err)
			continue
		}
		if !res.IsError {
			t.Errorf("%q evaluated to %q instead of being refused", expr, res.Content)
			continue
		}
		// The message has to tell the model what to write instead, or it will
		// just try the same thing again.
		if !strings.Contains(res.Content, "2*2*2") {
			t.Errorf("%q: message does not suggest an alternative: %s", expr, res.Content)
		}
	}
}

// Arithmetic that happens to be fine must still work.
func TestCalcStillAllowsOrdinaryArithmetic(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"7 % 4", "3"},
		{"10 % 3", "1"},
		{"(2+3)*4", "20"},
	} {
		args, _ := json.Marshal(calcArgs{Expr: tc.expr})
		res, _ := (Calc{}).Call(context.Background(), "c1", args)
		if res.IsError || res.Content != tc.want {
			t.Errorf("%q = %q (isError=%v), want %q", tc.expr, res.Content, res.IsError, tc.want)
		}
	}
}
