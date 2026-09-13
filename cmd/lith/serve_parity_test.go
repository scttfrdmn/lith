// SPDX-License-Identifier: Apache-2.0

package main

import (
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
	"small-file":          "FUSE prefetch policy (#69); gateway read path does not use it",
	"parts-max":           "FUSE small-file parts (#69); gateway read path does not use it",
	"footer-tier2":        "FUSE footer projection (#108); gateway read path does not use it",
	"sibling-readahead":   "FUSE sibling readahead (#63); gateway read path does not use it",
	"sibling-window":      "FUSE sibling detection (#63); gateway read path does not use it",
	"max-readahead":       "FUSE per-handle readahead window; gateway uses its per-client window",
	"bgzf-whole-file-max": "FUSE bgzf prefetch (#107); gateway read path does not use it",
	// Diagnostics / internal.
	"timeline-csv": "FUSE per-chunk diagnostic (#70)",
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
