package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// Calc evaluates an arithmetic expression.
//
// It has no side effects, so there is nothing to make idempotent on replay and
// nothing to gate behind approval. Those problems belong to a tool that actually
// writes something.
//
// Implementation note: go/types.Eval type-checks a constant expression against
// an empty scope. No package is loaded, so identifiers do not resolve and no
// code runs — "os.Exit(1)" is a type error, not an execution. Constant folding
// is exact rational arithmetic, so 240*0.15 is exactly 36, not 35.999999.
type Calc struct{}

func (Calc) Name() string { return "calc" }

func (Calc) Description() string {
	return "Evaluate an arithmetic expression and return the result. " +
		"Supports + - * / % and parentheses over integers and decimals. " +
		"Example expr: (240*0.15)+2"
}

func (Calc) Schema() json.RawMessage {
	// Hand-written on purpose. invopop/jsonschema arrives at tool five, once
	// writing these by hand actually hurts.
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "expr": {
      "type": "string",
      "description": "arithmetic expression, e.g. (240*0.15)+2"
    }
  },
  "required": ["expr"],
  "additionalProperties": false
}`)
}

// Call evaluates the expression. callID is ignored: calc has no side effects,
// so there is nothing downstream to make idempotent.
//
// Every failure here is one the model can fix, so they all come back as
// ToolResult{IsError: true} rather than as an error. The error return is
// reserved for "the tool could not be reached at all", which a pure function
// has no way to be — so it is always nil.
func (Calc) Call(_ context.Context, _ string, args json.RawMessage) (llm.ToolResult, error) {
	fail := func(format string, a ...any) (llm.ToolResult, error) {
		return llm.ToolResult{Content: fmt.Sprintf(format, a...), IsError: true}, nil
	}

	var in calcArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("calc: bad arguments: %v", err)
	}
	if in.Expr == "" {
		return fail("calc: expr is required")
	}

	tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, floatify(in.Expr))
	if err != nil {
		return fail("calc: cannot evaluate %q: %v", in.Expr, err)
	}
	if tv.Value == nil {
		return fail("calc: %q is not a constant expression", in.Expr)
	}
	return llm.ToolResult{Content: tv.Value.String()}, nil
}

type calcArgs struct {
	Expr string `json:"expr"`
}

// floatify rewrites integer literals as floats so the tool divides like a
// calculator instead of like Go.
//
// Go's untyped integer constants make 7/2 evaluate to 3 — correct language
// semantics, wrong answer for a model that asked for seven halves, and wrong
// *silently*, which is the failure mode that ruins an eval run. Rewriting
// 7/2 to 7.0/2.0 fixes it, and go/constant keeps exact rational arithmetic,
// so 240*0.15 is still exactly 36 rather than 35.999999999999996.
//
// Two escapes: % is integer-only in Go, so expressions using it keep integer
// semantics; and non-decimal literals (0x10, 0b11, 1_000) are left alone
// because appending ".0" to them is a syntax error. Any parse failure falls
// through unchanged so the real error surfaces from types.Eval.
func floatify(expr string) string {
	if strings.Contains(expr, "%") {
		return expr
	}
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return expr
	}

	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return true
		}
		if strings.ContainsAny(lit.Value, "xXbBoO_") || strings.HasPrefix(lit.Value, "0") && len(lit.Value) > 1 {
			return true // non-decimal or legacy-octal literal: leave it
		}
		lit.Kind = token.FLOAT
		lit.Value += ".0"
		return true
	})

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), node); err != nil {
		return expr
	}
	return buf.String()
}
