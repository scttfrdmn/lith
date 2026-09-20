// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// `go build ./cmd/<x>` writes its binary into the working directory, so anyone
// building from the repo root leaves an executable named after the command there —
// and `git add -A` then commits it. That happened twice: `lith-s3bench` (14.1 MB,
// #40/#48) and `lith-pfreplay` (2.8 MB, #267), together roughly 17 MB of a 42 MB
// pack. Neither was noticed at review, because a binary shows up in a diff as one
// unremarkable line.
//
// .gitignore now lists them, but .gitignore does nothing for a path that is already
// tracked — which is exactly how the second one got in while the first sat there. So
// the invariant is asserted instead: no file named after a cmd/ package is tracked.
func TestNoBuiltBinariesTracked(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	tracked := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			tracked[f] = true
		}
	}
	if len(tracked) == 0 {
		t.Fatal("git ls-files returned nothing; this guard is not actually checking anything")
	}

	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// The binary `go build ./cmd/<name>` produces, at the repo root.
		if tracked[e.Name()] {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("built binaries are tracked in git: %s\n"+
			"These are build artifacts, not sources. Untrack them with `git rm --cached <name>`; "+
			".gitignore alone will not help, because it is ignored for already-tracked paths.",
			strings.Join(found, ", "))
	}
}
