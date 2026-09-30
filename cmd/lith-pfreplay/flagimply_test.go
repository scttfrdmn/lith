// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A flag whose help text says "implies -global" must actually set it. `-mem-cache` shipped
// promising that and not doing it, so `-mem-cache 24GB` alone silently modelled nothing --
// the same defect as #264's flag that reached nothing, in the tool built to measure it.
//
// Parses main.go rather than running the binary: the claim is about the wiring, and a
// behavioural test would have to guess which of the two flags was broken.
func TestFlagsThatClaimToImplyGlobalDoSo(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Flag variables whose help string promises to imply -global.
	claims := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "Bool") && !strings.HasPrefix(sel.Sel.Name, "String") &&
			!strings.HasPrefix(sel.Sel.Name, "Int") {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "flag" {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if strings.Contains(lit.Value, "implies -global") {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					claims[id.Name] = false
				}
			}
		}
		return true
	})
	if len(claims) == 0 {
		t.Fatal("no flag help mentions \"implies -global\"; this guard has gone inert")
	}

	// Which of them are dereferenced in a condition that assigns global_ = true.
	ast.Inspect(f, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		var setsGlobal bool
		ast.Inspect(is.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 {
				return true
			}
			star, ok := as.Lhs[0].(*ast.StarExpr)
			if !ok {
				return true
			}
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "global_" {
				setsGlobal = true
			}
			return true
		})
		if !setsGlobal {
			return true
		}
		ast.Inspect(is.Cond, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok {
				if _, claimed := claims[id.Name]; claimed {
					claims[id.Name] = true
				}
			}
			return true
		})
		return true
	})

	for name, wired := range claims {
		if !wired {
			t.Errorf("flag %q says \"implies -global\" in its help but no `if ... { *global_ = true }` "+
				"references it: the flag silently does nothing on its own", name)
		}
	}
}
