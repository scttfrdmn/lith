// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// serveInapplicable documents every `lith mount` flag that is intentionally
// absent from `lith serve nfs`, with the reason. A mount flag that is neither
// present on serve nor listed here is a silently-missing option — the same class
// of bug as the auto-list fallback (#141) — and fails TestServeMountFlagParity.
var serveInapplicable = map[string]string{
	// FUSE mount presentation — the NFS gateway exports over NFSv3 (ownership via
	// AUTH_UNIX squash) and is a long-running server, not a mountable/daemonized FS.
	"allow-other": "FUSE mount option; NFS export uses AUTH_UNIX squash",
	"daemon":      "serve is a foreground long-running server",
	"exec":        "FUSE file-mode presentation; N/A over NFS",
	"uid":         "ownership via AUTH_UNIX/squash, not a fixed uid",
	"gid":         "ownership via AUTH_UNIX/squash, not a fixed gid",
	// FUSE read-path / prefetch-policy features. The NFS gateway read path uses its
	// own per-path concurrent block prefetch, not the FUSE prefetch policy, so these
	// have no effect on it.
	"small-file":               "FUSE prefetch policy (#69); gateway read path does not use it",
	"parts-max":                "FUSE small-file parts (#69); gateway read path does not use it",
	"footer-tier2":             "FUSE footer projection (#108); gateway read path does not use it",
	"sibling-readahead":        "FUSE sibling readahead (#63); gateway read path does not use it",
	"sibling-window":           "FUSE sibling detection (#63); gateway read path does not use it",
	"max-readahead":            "FUSE per-handle readahead window; gateway uses its per-client window",
	"readahead-evidence-ratio": "FUSE per-handle window evidence gate (#256); internal/nfs has no prefetcher at all",
	"prefetch-coverage-min":    "FUSE per-handle coverage gate (#221/#316); internal/nfs has no prefetcher at all",
	"bgzf-whole-file-max":      "FUSE bgzf prefetch (#107); gateway read path does not use it",
	// Diagnostics / internal.
	"timeline-csv": "FUSE per-chunk diagnostic (#70)",
	"pf-trace":     "FUSE access-pattern detector trace (#262); internal/nfs has no prefetcher at all",
	"pprof":        "debug surface; not part of the gateway operational surface",
	"disk-writers": "internal write-behind pool; fixed on serve",
	// (--keys is a `lith index build` flag, not a mount flag: build with
	// `lith index build --keys` then serve with --index-file.)
}

func flagNames(fs *pflag.FlagSet) map[string]bool {
	m := map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) { m[f.Name] = true })
	return m
}

// TestServeMountFlagParity asserts every `lith mount` flag that affects the read
// path, cache, budget, S3 client, or index is either present on `lith serve nfs`
// or documented as inapplicable (with a reason) in serveInapplicable.
func TestServeMountFlagParity(t *testing.T) {
	mount := flagNames(newMountCmd().Flags())
	serve := flagNames(newServeNFSCmd().Flags())

	var missing []string
	for name := range mount {
		if serve[name] {
			continue
		}
		if _, documented := serveInapplicable[name]; documented {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("mount flags neither present on `serve nfs` nor documented inapplicable: %s\n"+
			"add them to serve or add a reason to serveInapplicable", strings.Join(missing, ", "))
	}

	// Guard against stale documentation: an inapplicable entry must name a flag
	// that mount actually has and serve does not.
	for name := range serveInapplicable {
		if !mount[name] {
			t.Errorf("serveInapplicable lists %q, which is not a `lith mount` flag", name)
		}
		if serve[name] {
			t.Errorf("serveInapplicable lists %q, but `serve nfs` actually has it", name)
		}
	}
}

// serveConfigInapplicable documents every blockstore.Config field `lith mount` sets that
// `lith serve nfs` intentionally does not, with the reason. Empty is the goal.
var serveConfigInapplicable = map[string]string{
	// Recorder is set by both, under different names (the mount's timeline recorder vs the
	// gateway's metrics), so it is not listed here -- the key is what is compared.
}

// configKeysSetIn returns the blockstore.Config field names a file assigns.
func configKeysSetIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "blockstore" {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok {
					keys[k.Name] = true
				}
			}
		}
		return true
	})
	return keys
}

// ★ DERIVATION PARITY, which is the property TestServeMountFlagParity does not have and the
// one that would have caught #393.
//
// That test asserts a mount flag is either PRESENT on `serve nfs` or documented inapplicable.
// `--nic-gbps` and `--inflight-bytes` were both present, so it passed — while `runServeNFS`
// never called resolveNIC or computeInflightBytes and never passed a TTFB seed. A stock export
// therefore ran with NICBytesPerSec 0 (the coalesce gap pinned to its 256 KiB floor instead of
// the device figure), no TTFB seed (the gap floored on every cold prefix regardless), and
// InflightBytes 0 — which blockstore.Config documents as "0 disables byte gating", a state a
// mount cannot reach because computeInflightBytes never returns 0.
//
// The flags existed and nothing derived from them. So the gate has to compare what the two
// commands actually BUILD, not what they accept. Three missing derivations, three missing
// config keys — all three visible here.
func TestServeMountBlockStoreConfigParity(t *testing.T) {
	mount := configKeysSetIn(t, "cmd_mount.go")
	serve := configKeysSetIn(t, "serve.go")
	if len(mount) == 0 || len(serve) == 0 {
		t.Fatal("found no blockstore.Config literal in one of the commands; the AST walk is " +
			"broken, not the code")
	}

	var missing []string
	for name := range mount {
		if serve[name] {
			continue
		}
		if _, documented := serveConfigInapplicable[name]; documented {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("blockstore.Config fields `lith mount` sets and `lith serve nfs` does not: %s\n"+
			"A gateway left at a field's zero value is not the same as a mount at its derived "+
			"default — InflightBytes 0 disables byte gating, NICBytesPerSec 0 floors the "+
			"coalesce gap, TTFB 0 floors it on the cold prefix (#393). Set them on serve, or "+
			"add a reason to serveConfigInapplicable.", strings.Join(missing, ", "))
	}

	// Guard against stale documentation, same as the flag test.
	for name := range serveConfigInapplicable {
		if !mount[name] {
			t.Errorf("serveConfigInapplicable lists %q, which `lith mount` does not set", name)
		}
		if serve[name] {
			t.Errorf("serveConfigInapplicable lists %q, but `serve nfs` actually sets it", name)
		}
	}
}

// The three derivations themselves, named, because a config key can be present and assigned a
// zero literal. This is the narrower assertion that pins #393's actual fix.
func TestServeDerivesTheDeviceKnobs(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, tc := range []struct{ call, why string }{
		// ANCHORED ON THE PRIMARY RESOLUTION, not on the bare name. serve.go calls
		// resolveNIC twice -- once for the figure it uses and once with the override ignored,
		// for #239's comparison -- so grepping for "resolveNIC(" alone stayed green when the
		// primary call was replaced by a literal. Found by the revert sweep, not by reading.
		{"nic := resolveNIC(ctx, nicDir, f.nicGbps)",
			"without it NICBytesPerSec is 0 and the coalesce gap floors at 256 KiB"},
		{"computeInflightBytes(f.inflightBytes, nic.BaselineGbps)",
			"without it InflightBytes is 0, which disables byte gating"},
		{"logNICAttempts(", "flag parity with the mount's #317 diagnostics"},
		{"nicOverrideWarning(", "flag parity with the mount's #239 guard; an export is more exposed"},
	} {
		if !strings.Contains(s, tc.call) {
			t.Errorf("serve.go does not call %s — %s (#393)", tc.call, tc.why)
		}
	}
	// A TTFB seed that is literally zero is the same defect as not setting the field.
	if !strings.Contains(s, "TTFB:") {
		t.Error("serve.go sets no TTFB seed: MeasuredTTFB returns the seed until the first " +
			"fill lands, so the coalesce gap floors on every cold prefix (#393)")
	}
	if strings.Contains(s, "TTFB:     0") || strings.Contains(s, "TTFB: 0") {
		t.Error("serve.go sets TTFB to zero, which is indistinguishable from not setting it")
	}
}

// BOTH commands must validate --inflight-bytes. The shared derivation
// (computeInflightBytes) falls through on a parse failure, so a command that does not
// validate first accepts a typo, discards it, and logs a derived budget as though the flag had
// never been passed (#393). serve always errored; the mount never did, and unifying the
// derivation would have been a chance to lose serve's half rather than gain mount's.
func TestBothCommandsValidateInflightBytes(t *testing.T) {
	for _, path := range []string{"cmd_mount.go", "serve.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "validateInflightBytes(f.inflightBytes)") {
			t.Errorf("%s does not validate --inflight-bytes: a malformed value would be "+
				"silently replaced by the NIC-derived default, with nothing saying the flag "+
				"was ignored (#393, the #264 class)", path)
		}
	}
}
