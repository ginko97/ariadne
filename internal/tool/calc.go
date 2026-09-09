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
)

// Calc evaluates an arithmetic expression.
//
// It is the right first tool because it has no side effects: nothing to make
// idempotent in week 5, nothing to gate behind approval in week 9. Save those
// problems for a tool that actually writes something.
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

func (Calc) Call(_ context.Context, args json.RawMessage) (string, error) {
	var in calcArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("calc: bad arguments: %w", err)
	}
	if in.Expr == "" {
		return "", fmt.Errorf("calc: expr is required")
	}

	tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, floatify(in.Expr))
	if err != nil {
		return "", fmt.Errorf("calc: cannot evaluate %q: %w", in.Expr, err)
	}
	if tv.Value == nil {
		return "", fmt.Errorf("calc: %q is not a constant expression", in.Expr)
	}
	return tv.Value.String(), nil
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
type calcArgs struct {
	Expr string `json:"expr"`
}

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
