// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// #301: the readahead window is clamped by what the budget could hold, and nothing else.
func TestWindowForBudget(t *testing.T) {
	cases := []struct {
		name                string
		maxRA, budgetBlocks int64
		want                int64
	}{
		// The regime the divisor was wrong about: plenty of budget, so the configured
		// window stands. Under the divisor with 256 open descriptors this was 2.
		{"budget has room", 223, 492, 223},
		// The budget is a ceiling: don't hand out a window it could never hold.
		{"budget is the ceiling", 223, 60, 60},
		// The floor every handle gets regardless.
		{"floor", 223, 1, 2},
		{"floor from maxRA", 1, 492, 2},
		// No budget configured must not collapse the window to the floor.
		{"no budget", 223, 0, 223},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := windowForBudget(c.maxRA, c.budgetBlocks); got != c.want {
				t.Errorf("windowForBudget(%d, %d) = %d, want %d",
					c.maxRA, c.budgetBlocks, got, c.want)
			}
		})
	}
}

// The divisor must not creep back. perHandleWindow dividing by the open-handle count cost a
// measured 6-10x wall clock on a reader holding descriptors open, and the error was invisible
// because nothing reported the realized window. Asserted structurally rather than
// behaviourally: a behavioural test needs a store and would not say WHY it regressed.
func TestPerHandleWindowDoesNotConsultTheHandleCount(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse fs.go: %v", err)
	}
	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name.Name == "perHandleWindow" {
			fn = d
		}
		return true
	})
	if fn == nil {
		t.Fatal("perHandleWindow not found: this guard has gone inert")
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "handles" {
			return true
		}
		t.Error("perHandleWindow references f.handles again: the readahead window must not be " +
			"divided by the open-descriptor count (#301). Aggregate readahead is bounded by " +
			"BlockStore.admitCommitted, byte-exactly, where the quantity is measured.")
		return false
	})
}
