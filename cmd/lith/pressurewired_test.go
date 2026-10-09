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
)

// THE POLICY MUST ACTUALLY BE IN THE PATH, which the policy tests cannot show.
//
// pressuremax_test.go proves pressureMaxFor decides correctly. None of it would fail if the
// two commands went back to passing the RAW FLAG into the blockstore -- `PrefetchPressureMax:
// f.prefetchPressure` -- because f.prefetchPressure is still read by pressureMaxWarning, so
// even TestEveryFlagBindingIsConsumed stays green. The derived default would then be dead code
// that every unit test passes.
//
// That is not hypothetical. The ttfbRecorder hook shipped exactly this way: declared, never
// called, with a clean build, a clean lint, a green `go test ./...` and a correct-looking
// gauge beside it. The only check that catches it is one that asserts the call site.
//
// Same AST technique as TestEveryFlagBindingIsConsumed (#264), narrowed to one field: every
// `PrefetchPressureMax:` in a blockstore.Config literal must take a LOCAL IDENTIFIER (the
// policy's result), never a selector like `f.prefetchPressure`, and every file that sets it
// must call pressureMaxFor.
func TestPressureMaxReachesTheBlockStore(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	assignments := 0
	var raw, missingCall []string

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

		setsField, callsPolicy := false, false
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "pressureMaxFor" {
					callsPolicy = true
				}
			}
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "PrefetchPressureMax" {
				return true
			}
			setsField = true
			assignments++
			// A selector here is the regression: the flag going straight through, with the
			// policy bypassed. A local identifier is what the policy's result looks like.
			if sel, ok := kv.Value.(*ast.SelectorExpr); ok {
				raw = append(raw, path+": PrefetchPressureMax: "+exprName(sel))
			}
			return true
		})
		if setsField && !callsPolicy {
			missingCall = append(missingCall, path)
		}
	}

	// Both command paths set it: `lith mount` and `lith serve nfs`. If this drops to one, a
	// path stopped configuring the gate at all -- which is how the gateway shipped without
	// pressureMaxWarning in the first place.
	if assignments < 2 {
		t.Errorf("found %d PrefetchPressureMax assignments, want at least 2 (mount and serve "+
			"nfs) — a command path is no longer configuring the pressure gate", assignments)
	}
	sort.Strings(raw)
	if len(raw) > 0 {
		t.Errorf("PrefetchPressureMax assigned from a struct field, bypassing pressureMaxFor — "+
			"the region-derived default is dead and the raw flag is in force:\n  %s",
			strings.Join(raw, "\n  "))
	}
	sort.Strings(missingCall)
	if len(missingCall) > 0 {
		t.Errorf("file(s) configure PrefetchPressureMax without calling pressureMaxFor: %s",
			strings.Join(missingCall, ", "))
	}
}

// exprName renders a selector for an error message: `f.prefetchPressure`.
func exprName(sel *ast.SelectorExpr) string {
	if x, ok := sel.X.(*ast.Ident); ok {
		return x.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}

// The mount path must ALSO read the evidence gate's own policy function rather than re-deciding
// from nearRegion/regionKnown, which is the drift pressureMaxFor's doc comment is about: two
// policies over the same two booleans can disagree, and the failure would be silent (both gates
// off, or both on, with nothing in the log looking wrong).
func TestTheMountDerivesPressureFromTheEvidencePolicy(t *testing.T) {
	src, err := os.ReadFile("cmd_mount.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "EvidenceRatioFor(") {
		t.Error("cmd_mount.go does not call EvidenceRatioFor; the pressure default must be " +
			"derived from the evidence gate's RESULT, not from a second reading of the region pair")
	}
	if !strings.Contains(s, "pressureMaxFor(") {
		t.Error("cmd_mount.go does not call pressureMaxFor")
	}
}

// The headroom check must be IN both command paths, which the pure-function tests cannot show.
// Same reason as TestPressureMaxReachesTheBlockStore: a warning nobody calls is a warning that
// does not exist, and it builds, lints and tests clean.
func TestMemTierHeadroomIsCheckedByBothCommands(t *testing.T) {
	for _, path := range []string{"cmd_mount.go", "serve.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "memTierHeadroomWarning(") {
			t.Errorf("%s never calls memTierHeadroomWarning: a 20 GB --mem-cache on a 33 GB "+
				"box would mount silently and be OOM-killed (#314)", path)
		}
	}
}

// The NIC chain's failure reasons must be SURFACED, not merely collected (#317). resolveNIC
// filling in nicInfo.Tried changes nothing on its own; the reporter's ask was that the log and
// doctor say why each source failed, and a collected-but-unreported field is the shape that
// shipped dead once already (the ttfbRecorder hook).
func TestNICAttemptsAreReported(t *testing.T) {
	for _, tc := range []struct{ path, call string }{
		{"cmd_mount.go", "logNICAttempts("},
		{"cmd_doctor.go", "addNICAttempts("},
	} {
		src, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), tc.call) {
			t.Errorf("%s does not reference %s: the NIC chain would collect its failure "+
				"reasons and report none of them, which is exactly the invisibility #317 was "+
				"filed about", tc.path, tc.call)
		}
	}
}

// The peak-vs-baseline warning must fire where the MISTAKE IS MADE (#239). It lived only in
// `lith doctor`, which is opt-in, while the error is committed on the mount command line --
// the same one-consumer gap as #240/#242/#346/#393. A pure function that both commands could
// call proves nothing about whether the mount calls it.
func TestNICOverrideWarningIsCheckedWhereTheMistakeIsMade(t *testing.T) {
	for _, path := range []string{"cmd_mount.go", "cmd_doctor.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "nicOverrideWarning(") {
			t.Errorf("%s never calls nicOverrideWarning: passing the advertised peak to "+
				"--nic-gbps fetched 654 MB against 382 MB for the same data, and nothing on "+
				"this path would say so (#239)", path)
		}
	}
	// And it must re-resolve with the override IGNORED. Comparing the override against itself
	// is vacuous, and resolveNIC short-circuits on it, so a call that forwarded f.nicGbps
	// would always see baseline == peak == the override and never warn.
	src, err := os.ReadFile("cmd_mount.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "nicOverrideWarning(f.nicGbps, resolveNIC(ctx, nicDir, 0))") {
		t.Error("the mount does not compare against a detection that ignores the override; " +
			"resolveNIC short-circuits on --nic-gbps, so any other argument makes the " +
			"comparison vacuous")
	}
}
