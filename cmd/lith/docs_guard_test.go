// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// currentMajorMinor is the release line these docs describe. Bump it when the
// minor changes; the version-residue guard below flags any pre-1.0 tag/URL that
// slips back into user-facing docs.
const currentMajorMinor = "1.0"

// userDocs returns the user-facing docs the drift guards police: the README, the
// security policy, and every docs/ page. CHANGELOG.md is excluded — it is the one
// place historical version references belong.
func userDocs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(rel string) {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		out[rel] = string(b)
	}
	add("README.md")
	add("SECURITY.md")
	pages, _ := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		out["docs/"+filepath.Base(p)] = string(b)
	}
	return out
}

// TestReadmeListsEveryCommand: the README command summary must list every
// top-level command, so a command shipped without a README line fails CI. This
// extends TestKnobsDocumentsEveryFlag from flags to commands (#198).
func TestReadmeListsEveryCommand(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	doc := string(readme)
	skip := map[string]bool{"help": true, "completion": true}
	var missing []string
	for _, c := range newRootCmd().Commands() {
		name := c.Name()
		if skip[name] {
			continue
		}
		if !strings.Contains(doc, "lith "+name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("README command summary is missing: %s\nAdd a `lith <cmd>` line to the ## Commands block.", strings.Join(missing, " "))
	}
}

// TestNoPre1xStatusInDocs: user-facing docs must not describe lith as pre-1.0.
// These phrases are unambiguous status residue (not benchmark numbers), so the
// check is safe to automate.
func TestNoPre1xStatusInDocs(t *testing.T) {
	banned := []string{"pre-1.0", "v0.x", "before v1", "no compatibility promise"}
	for name, body := range userDocs(t) {
		low := strings.ToLower(body)
		for _, phrase := range banned {
			if strings.Contains(low, strings.ToLower(phrase)) {
				t.Errorf("%s still contains pre-1.0 status language %q", name, phrase)
			}
		}
	}
}

// TestNoStaleVersionTagsInDocs: no user-facing doc may reference a pre-current
// image tag, tarball name, or release/module version — the residue an external
// review found (`ghcr.io/…/lith:0.5`). Scoped to tag/URL/module contexts so it
// does not false-positive on benchmark values like "0.5 s" or "0.13 GB"; general
// version numbers in prose are left to human review (too brittle to automate
// without flagging every measured latency).
func TestNoStaleVersionTagsInDocs(t *testing.T) {
	// image tag `lith:0.`, binary `lith_0.`, release URL `/v0.`, module `@v0.`.
	stale := regexp.MustCompile(`lith:0\.|lith_0\.|/v0\.|@v0\.`)
	for name, body := range userDocs(t) {
		if m := stale.FindString(body); m != "" {
			t.Errorf("%s references a stale pre-%s version tag/URL (%q); update it to the current release line",
				name, currentMajorMinor, m)
		}
	}
}
