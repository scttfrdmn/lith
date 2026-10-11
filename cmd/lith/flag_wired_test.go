// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A flag can be declared, documented, and silently never read — which is what
// happened to `--pf-trace` (#264): it bound a struct field that nothing else in the
// package ever mentioned, so it produced no file, no error and exit 0, and the
// docs guard passed because the flag *was* documented. `docs_flags_test.go` catches
// an undocumented flag; nothing caught an unconsumed one.
//
// This walks the package's AST for flag registrations of the form
// `fl.XxxVar(&f.field, "name", ...)` and requires each bound field to be
// referenced somewhere other than its own registration. That is a weak
// property — it cannot prove the value reaches the right place — but it is exactly
// the property whose absence made #264 invisible, and it costs nothing.
func TestEveryFlagBindingIsConsumed(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	// field name -> (registrations, other references)
	type usage struct{ decls, refs int }
	seen := map[string]*usage{}
	flagOf := map[string]string{} // field -> flag name, for the error message

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		// Pass 1: flag registrations — fl.StringVar(&f.pfTrace, "pf-trace", ...).
		regArg := map[ast.Node]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(sel.Sel.Name, "Var") {
				return true
			}
			unary, ok := call.Args[0].(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				return true
			}
			field, ok := unary.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name := field.Sel.Name
			if seen[name] == nil {
				seen[name] = &usage{}
			}
			seen[name].decls++
			flagOf[name] = strings.Trim(lit.Value, `"`)
			regArg[field] = true // don't count the registration itself as a use
			return true
		})

		// Pass 2: every other reference to those fields.
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || regArg[sel] {
				return true
			}
			if u := seen[sel.Sel.Name]; u != nil {
				u.refs++
			}
			return true
		})
	}

	if len(seen) == 0 {
		t.Fatal("found no flag registrations; the AST walk is broken, not the code")
	}

	var unconsumed []string
	for field, u := range seen {
		if u.refs == 0 {
			unconsumed = append(unconsumed, "--"+flagOf[field]+" (binds f."+field+")")
		}
	}
	sort.Strings(unconsumed)
	if len(unconsumed) > 0 {
		t.Errorf("flag(s) bound to a field nothing ever reads — declared, documented, and silently ignored:\n  %s\n"+
			"Wire the field through to the config it belongs in (#264).", strings.Join(unconsumed, "\n  "))
	}
}

// TestCheckPFTracePathFailsFast: --pf-trace is a diagnostic the operator asked for
// by name, and under --daemon a failure to open it used to be both non-fatal and
// invisible — the mount succeeded and the reason went to /tmp/lith-<uid>-mount.log,
// so a whole characterization job ran and produced nothing (#264). Validation now
// happens in the foreground, before the fork.
func TestCheckPFTracePathFailsFast(t *testing.T) {
	if err := checkPFTracePath(""); err != nil {
		t.Errorf("empty path means tracing off, not an error: %v", err)
	}
	ok := filepath.Join(t.TempDir(), "pf.csv")
	if err := checkPFTracePath(ok); err != nil {
		t.Errorf("writable path rejected: %v", err)
	}
	if _, err := os.Stat(ok); err != nil {
		t.Errorf("validation should leave the file the mount will write: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "no-such-dir", "pf.csv")
	err := checkPFTracePath(bad)
	if err == nil {
		t.Fatal("an unwritable --pf-trace path must fail the mount, not mount successfully without a trace")
	}
	if !strings.Contains(err.Error(), "pf-trace") {
		t.Errorf("error should name the flag so the cause is obvious: %v", err)
	}
}

// AN EXPERIMENTAL FLAG MUST DEFAULT OFF, and nothing asserted it until a revert sweep flipped
// one to `true` and the whole suite stayed green.
//
// "EXPERIMENTAL" in a flag's help is a promise that the behaviour behind it has not been
// measured enough to be anyone's default. This project has withdrawn two defaults that were
// placed from a plausible-looking constant (#340, #349) and has a standing rule that a default
// needs a measurement behind it, so a flag that says EXPERIMENTAL and ships on is a
// contradiction an operator cannot see.
//
// Keyed on the help text rather than a list, so a new experimental flag is covered the moment
// it is written — a list would need maintaining and the thing being guarded is exactly the kind
// of change that forgets to maintain lists.
func TestExperimentalFlagsDefaultOff(t *testing.T) {
	checked := 0
	for _, cmd := range []*cobra.Command{newMountCmd(), newServeNFSCmd()} {
		name := cmd.Name()
		cmd.Flags().VisitAll(func(fl *pflag.Flag) {
			if !strings.Contains(strings.ToUpper(fl.Usage), "EXPERIMENTAL") {
				return
			}
			checked++
			switch fl.DefValue {
			case "false", "0", "", "0s":
				return
			}
			t.Errorf("%s --%s says EXPERIMENTAL in its help but defaults to %q. An "+
				"experimental default is a contradiction: the help tells an operator the "+
				"behaviour is unmeasured while the binary ships it on. Either measure it and "+
				"drop the word, or default it off.", name, fl.Name, fl.DefValue)
		})
	}
	if checked == 0 {
		t.Fatal("found no flags whose help says EXPERIMENTAL; the check is broken, not the code")
	}
	t.Logf("checked %d experimental flag(s)", checked)
}
