// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ★ THE SPAN'S OFFSET MUST COME FROM THE DISPATCH, and only an AST check can see it.
//
// A span dispatch carries an ABSOLUTE range (SpanOff, SpanLen) because an extension starts where
// the previous span ended, not where the current read ends. Passing the read's end instead --
// which is what an earlier version of this code did, when a span was placed rather than extended
// -- makes the fetch SHORTER than the frontier claims: it covers [end, end+len) when it should
// cover [off, off+len) with off > end, so the lead silently falls behind what the detector
// recorded. That is the shipped regression's shape.
//
// NO BEHAVIOURAL TEST IN THIS PACKAGE CATCHES IT, and a revert sweep proved it: swapping
// pd.SpanOff for `end` leaves every test green. The bytes come out the same because the
// blockstore dedups a range it has already fetched, and the fault counters are too noisy in a
// fake-server fixture to assert on. So this asserts the wiring directly, in the same shape as
// cmd/lith's TestPressureMaxReachesTheBlockStore.
func TestSpanDispatchPassesItsOwnOffset(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fs.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PrefetchRange" {
			return true
		}
		// PrefetchRange(ctx, key, off, length, objSize). Only the dispatch-loop call that
		// carries a SpanLen is in scope here; the strided lane has its own offset rule.
		if len(call.Args) < 5 {
			return true
		}
		lenArg := exprText(fset, call.Args[3])
		if !strings.Contains(lenArg, "SpanLen") {
			return true
		}
		calls++
		if offArg := exprText(fset, call.Args[2]); !strings.Contains(offArg, "SpanOff") {
			bad = append(bad, offArg)
		}
		return true
	})

	if calls == 0 {
		t.Fatal("found no PrefetchRange call taking a SpanLen; this check is broken, not the " +
			"code -- either the span dispatch was removed or the field was renamed")
	}
	for _, offArg := range bad {
		t.Errorf("a span's PrefetchRange takes its offset from %q, not the dispatch's SpanOff. "+
			"A span extension begins where the previous span ended, so any other offset makes "+
			"the fetch fall short of the frontier the detector recorded, and the coverage gap "+
			"that caused a 20%% wall regression reopens quietly.", offArg)
	}
	t.Logf("checked %d span PrefetchRange call(s)", calls)
}

func exprText(fset *token.FileSet, e ast.Expr) string {
	start := fset.Position(e.Pos())
	end := fset.Position(e.End())
	src := mustReadFS(start.Filename)
	return src[start.Offset:end.Offset]
}

func mustReadFS(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}
