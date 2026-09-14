// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docsFlagAllowlist are flags deliberately not documented in knobs.md: cobra's
// auto-added help, and flags whose home is a dedicated docs page rather than the
// knobs reference. Keep this list short and justified.
var docsFlagAllowlist = map[string]bool{
	"help": true, // cobra auto-adds --help everywhere
}

// TestKnobsDocumentsEveryFlag is the drift guard (#179): every flag on every lith
// command must be mentioned in docs/knobs.md, so a flag added without a docs entry
// fails CI. Add the flag to knobs.md (or, with justification, to the allowlist).
func TestKnobsDocumentsEveryFlag(t *testing.T) {
	knobs, err := os.ReadFile(filepath.Join("..", "..", "docs", "knobs.md"))
	if err != nil {
		t.Fatalf("read knobs.md: %v", err)
	}
	doc := string(knobs)

	seen := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) { seen[f.Name] = true })
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())

	var missing []string
	for name := range seen {
		if docsFlagAllowlist[name] {
			continue
		}
		if !strings.Contains(doc, "--"+name) {
			missing = append(missing, "--"+name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/knobs.md is missing %d shipped flag(s): %s\n"+
			"Add each to docs/knobs.md (or, with justification, to docsFlagAllowlist).",
			len(missing), strings.Join(missing, " "))
	}
}
