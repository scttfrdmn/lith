// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// #314: the memory budget is the cgroup limit when there is one, not the host's MemTotal.
//
// Inside a container /proc/meminfo is not namespaced by the memory controller, so it reports the
// HOST's RAM. Before this, a container limited to 4 GiB on a 256 GiB host got a default tier of
// 64 GiB -- sixteen times its whole allowance -- and the headroom warning, comparing against the
// same figure, stayed silent. The gateway's documented deployment is that container.
func TestMemBudgetIsTheTighterOfHostAndCgroup(t *testing.T) {
	const gib = int64(1) << 30
	const host = 256 * gib
	const v1Unlimited = "9223372036854771712" // what cgroup v1 writes for "no limit" on x86-64

	cases := []struct {
		name     string
		memTotal int64
		v2, v1   string
		want     int64
	}{
		{"bare metal, no cgroup files", host, "", "", host},
		{"cgroup v2, limited", host, "4294967296", "", 4 * gib},
		{"cgroup v2, unlimited is the literal max", host, "max", "", host},
		{"cgroup v1, limited", host, "", "8589934592", 8 * gib},
		{"cgroup v1, unlimited is a near-MaxInt64", host, "", v1Unlimited, host},
		{"a limit above physical RAM does not raise the budget", 16 * gib, "34359738368", "", 16 * gib},
		{"both present, the tighter wins", host, "8589934592", "4294967296", 4 * gib},
		{"malformed limit is ignored, not trusted", host, "lots", "", host},
		{"no MemTotal, cgroup limit still counts", 0, "4294967296", "", 4 * gib},
		{"no MemTotal, v1 unlimited is not mistaken for a budget", 0, "", v1Unlimited, 0},
		{"nothing readable at all", 0, "", "", 0},
	}
	for _, c := range cases {
		if got := memBudgetFrom(c.memTotal, c.v2, c.v1); got != c.want {
			t.Errorf("%s: memBudgetFrom(%d, %q, %q) = %d, want %d",
				c.name, c.memTotal, c.v2, c.v1, got, c.want)
		}
	}

	// THE CONSEQUENCE THAT WAS THE BUG, stated as the default tier rather than the budget, so a
	// future edit that computes the budget correctly and then sizes the tier from MemTotal anyway
	// is caught too.
	budget := memBudgetFrom(host, "4294967296", "")
	if tier := memCacheDefaultFor(budget); tier > budget/2 {
		t.Errorf("in a 4 GiB container on a 256 GiB host the default tier is %d bytes, over half "+
			"the container's allowance", tier)
	}
}

// THE AUTOMATIC LIMIT ENGAGES ONLY WHERE THE TIER IS ALREADY PREDICTED NOT TO FIT.
//
// The property that lets it default on: it changes nothing for a configuration that was fine.
// #314's first round recommended GOMEMLIMIT without setting it, because the right value depends
// on what else is on the box; that objection is about imposing a limit on every mount, and does
// not survive the case where the box is already predicted to OOM.
func TestAutoMemoryLimitOnlyWhereTheTierCannotFit(t *testing.T) {
	const gib = int64(1) << 30

	// The default tier must NEVER engage the limit, at any box size. Swept, because a fraction
	// that holds at one size and not another is exactly the kind of thing a single case misses.
	for _, budget := range []int64{2 * gib, 8 * gib, 32 * gib, 256 * gib, 2048 * gib} {
		tier := memCacheDefaultFor(budget)
		got, err := memoryLimitFor("auto", false, tier, budget)
		if err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("budget %d GiB, default tier %d GiB: auto set a limit of %d bytes. The "+
				"default configuration must behave exactly as it did before this change",
				budget/gib, tier/gib, got)
		}
	}

	// The boundary is the headroom warning's own condition, 2 x tier >= budget, so the warning
	// and the limit cannot disagree about which configurations are at risk.
	const budget = 32 * gib
	for _, c := range []struct {
		tier    int64
		engages bool
	}{
		{budget/2 - 1, false}, // just fits
		{budget / 2, true},    // the warning's threshold
		{budget * 6 / 10, true},
	} {
		got, err := memoryLimitFor("auto", false, c.tier, budget)
		if err != nil {
			t.Fatal(err)
		}
		if (got > 0) != c.engages {
			t.Errorf("tier %d of a %d budget: auto limit %d, engages=%v, want %v",
				c.tier, budget, got, got > 0, c.engages)
		}
		warns := memTierHeadroomWarning(c.tier, budget, 0) != ""
		if warns != c.engages {
			t.Errorf("tier %d: the headroom warning fires=%v but the auto limit engages=%v; "+
				"they must agree on which configurations are at risk", c.tier, warns, c.engages)
		}
		if c.engages {
			b := budget // a variable, so the product is computed at run time like the code's
			if want := int64(memLimitHeadroom * float64(b)); got != want {
				t.Errorf("tier %d: auto limit %d, want %d (%.0f%% of the budget)",
					c.tier, got, want, 100*memLimitHeadroom)
			}
			if got >= budget {
				t.Errorf("auto limit %d is not under the %d budget it exists to stay inside",
					got, budget)
			}
		}
	}
}

// An explicit GOMEMLIMIT wins over "auto", an explicit flag wins over both, and "off" is off.
func TestMemoryLimitPrecedence(t *testing.T) {
	const gib = int64(1) << 30
	const budget = 32 * gib
	const riskyTier = 20 * gib // over half: auto would engage

	got, _ := memoryLimitFor("auto", true, riskyTier, budget)
	if got != 0 {
		t.Errorf("with GOMEMLIMIT set, auto returned %d; the runtime has already applied the "+
			"operator's value before main ran and lith must not override it", got)
	}
	for _, off := range []string{"off", "0", "OFF"} {
		if got, _ := memoryLimitFor(off, false, riskyTier, budget); got != 0 {
			t.Errorf("--memory-limit %s returned %d, want 0", off, got)
		}
	}
	got, err := memoryLimitFor("24GiB", true, riskyTier, budget)
	if err != nil {
		t.Fatal(err)
	}
	if got != 24*gib {
		t.Errorf("--memory-limit 24GiB returned %d; an explicit flag must win, including over "+
			"GOMEMLIMIT", got)
	}
	if _, err := memoryLimitFor("plenty", false, riskyTier, budget); err == nil {
		t.Error("--memory-limit plenty was accepted; a value that is not a size, auto or off " +
			"must be an error rather than silently meaning no limit")
	}
}

// A limit at or below the tier cannot be met: the tier is live heap the collector cannot free.
func TestMemoryLimitBelowTheTierWarns(t *testing.T) {
	const gib = int64(1) << 30
	if w := memoryLimitWarning(10*gib, 12*gib); w == "" {
		t.Error("a 10 GiB limit under a 12 GiB tier did not warn")
	}
	if w := memoryLimitWarning(12*gib, 12*gib); w == "" {
		t.Error("a limit equal to the tier did not warn")
	}
	if w := memoryLimitWarning(16*gib, 12*gib); w != "" {
		t.Errorf("a 16 GiB limit over a 12 GiB tier warned: %s", w)
	}
	if w := memoryLimitWarning(0, 12*gib); w != "" {
		t.Errorf("no limit warned: %s", w)
	}
	// And the automatic limit can never trip this on its own: it engages only at
	// tier >= budget/2, and sits at 0.9 x budget, so tier < limit until tier exceeds 90% of
	// the budget -- a configuration the operator typed, which then deserves the warning.
	const budget = 32 * gib
	for _, tier := range []int64{budget / 2, budget * 7 / 10, budget * 89 / 100} {
		limit, _ := memoryLimitFor("auto", false, tier, budget)
		if w := memoryLimitWarning(limit, tier); w != "" {
			t.Errorf("tier %d: the automatic limit %d tripped its own warning", tier, limit)
		}
	}
}

// BOTH COMMANDS MUST APPLY THE LIMIT, and only an AST check can see it.
//
// A behavioural test cannot: the limit is process-global runtime state set inside each command's
// run function, which no unit test executes end to end. This project has seven recorded cases of
// the mount and the gateway wiring one mechanism differently, and the gateway is both the
// container image's entrypoint -- where the cgroup budget matters -- and the place an OOM takes
// every client down. So: every file that sizes a memory tier must also call applyMemoryLimit and
// must pass memBudgetBytes, not totalMemBytes, to the headroom warning.
func TestEveryCommandAppliesTheMemoryBudgetAndLimit(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	type use struct{ sizesTier, applies, budget, hostOnly bool }
	uses := map[string]*use{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "cachecheck.go" {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		u := &use{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "defaultMemCacheBytes":
				u.sizesTier = true
			case "applyMemoryLimit":
				u.applies = true
			case "memBudgetBytes":
				u.budget = true
			case "totalMemBytes":
				u.hostOnly = true
			}
			return true
		})
		if u.sizesTier || u.applies || u.hostOnly {
			uses[path] = u
		}
	}

	sizing := 0
	for path, u := range uses {
		if u.hostOnly {
			t.Errorf("%s calls totalMemBytes directly; that is the HOST's RAM inside a container. "+
				"Use memBudgetBytes, which takes the cgroup limit into account", path)
		}
		if !u.sizesTier {
			continue
		}
		sizing++
		if !u.applies {
			t.Errorf("%s sizes a memory tier but never calls applyMemoryLimit: that command would "+
				"run without the soft limit the other applies", path)
		}
		if !u.budget {
			t.Errorf("%s sizes a memory tier but never calls memBudgetBytes", path)
		}
	}
	if sizing < 2 {
		t.Fatalf("found %d file(s) sizing a memory tier, want both the mount and the gateway; "+
			"this check is broken, not the code", sizing)
	}
}

// THE DEFAULT TIER ITSELF MUST BE SIZED FROM THE BUDGET, which is the original bug and was the
// one revert nothing caught.
//
// The check above walks the commands and skips cachecheck.go, because that file legitimately
// calls totalMemBytes -- memBudgetBytes is built on it. But defaultMemCacheBytes lives there
// too, and reverting it to `memCacheDefaultFor(totalMemBytes())` reintroduced exactly the
// container OOM this change exists to fix while every test stayed green. So the function is
// checked by name: its body must reach memBudgetBytes and must not reach totalMemBytes.
func TestDefaultTierIsSizedFromTheBudget(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cachecheck.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "defaultMemCacheBytes" {
			continue
		}
		found = true
		var budget, host bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					budget = budget || id.Name == "memBudgetBytes"
					host = host || id.Name == "totalMemBytes"
				}
			}
			return true
		})
		if host {
			t.Error("defaultMemCacheBytes reads totalMemBytes, which is the HOST's RAM inside a " +
				"container: a 4 GiB container on a 256 GiB host would default to a 64 GiB tier")
		}
		if !budget {
			t.Error("defaultMemCacheBytes does not size the tier from memBudgetBytes")
		}
	}
	if !found {
		t.Fatal("no defaultMemCacheBytes in cachecheck.go; this check is broken, not the code")
	}
}
