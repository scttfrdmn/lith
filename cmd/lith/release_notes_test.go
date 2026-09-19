// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scripts/release-notes.sh supplies the GitHub release body from CHANGELOG.md
// (goreleaser's own changelog generation is disabled, and with nothing in its
// place every release from v0.5.0 on shipped an empty body). The release workflow
// already fails loudly when the section is missing or the published body is
// empty; what those runtime guards cannot catch is the extraction silently
// returning the *wrong* section. These tests pin that.
func runNotes(t *testing.T, changelog, tag string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script; release runs on linux")
	}
	script := filepath.Join("..", "..", "scripts", "release-notes.sh")
	out, err := exec.Command("bash", script, tag, changelog).Output()
	return string(out), err
}

const fixtureChangelog = `# Changelog

## [Unreleased]

- unreleased work that must never appear in a release body

## [2.1.0] - 2026-01-02

Second entry preamble.

### Added

- the 2.1.0 thing

## [2.0.0] - 2026-01-01

### Fixed

- the 2.0.0 thing
`

func writeFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(p, []byte(fixtureChangelog), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReleaseNotesExtractsExactSection: the body is this version's section only —
// it stops at the next heading and never bleeds in [Unreleased] or an older entry.
func TestReleaseNotesExtractsExactSection(t *testing.T) {
	got, err := runNotes(t, writeFixture(t), "v2.1.0")
	if err != nil {
		t.Fatalf("release-notes.sh: %v", err)
	}
	for _, want := range []string{"Second entry preamble.", "the 2.1.0 thing"} {
		if !strings.Contains(got, want) {
			t.Errorf("body is missing %q; got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Unreleased", "unreleased work", "the 2.0.0 thing", "## ["} {
		if strings.Contains(got, unwanted) {
			t.Errorf("body leaked %q; got:\n%s", unwanted, got)
		}
	}
	if strings.HasPrefix(got, "\n") {
		t.Errorf("body starts with a blank line; got %q", got)
	}
}

// TestReleaseNotesMissingSectionFails: a tag with no CHANGELOG section must exit
// non-zero so the release fails instead of publishing an empty body.
func TestReleaseNotesMissingSectionFails(t *testing.T) {
	if _, err := runNotes(t, writeFixture(t), "v9.9.9"); err == nil {
		t.Fatal("release-notes.sh succeeded for a version with no CHANGELOG section, want failure")
	}
}

// TestReleaseNotesPrereleaseFallsBackToBase: an rc is cut from the same CHANGELOG
// state as the release it rehearses, so it has no section of its own and must
// fall back to the base version rather than failing the rc.
func TestReleaseNotesPrereleaseFallsBackToBase(t *testing.T) {
	got, err := runNotes(t, writeFixture(t), "v2.1.0-rc1")
	if err != nil {
		t.Fatalf("release-notes.sh for a prerelease: %v", err)
	}
	if !strings.Contains(got, "the 2.1.0 thing") {
		t.Errorf("prerelease body did not fall back to [2.1.0]; got:\n%s", got)
	}
}

// TestReleaseNotesRealChangelogHasCurrentSection: the real CHANGELOG must yield a
// non-trivial body for its newest released version, so the next tag cut from this
// tree cannot publish empty.
func TestReleaseNotesRealChangelogHasCurrentSection(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	var ver string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## [") && !strings.HasPrefix(line, "## [Unreleased]") {
			ver = strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(line, "## ["), "]", 2)[0], "]")
			break
		}
	}
	if ver == "" {
		t.Fatal("no released section found in CHANGELOG.md")
	}
	got, err := runNotes(t, filepath.Join("..", "..", "CHANGELOG.md"), "v"+ver)
	if err != nil {
		t.Fatalf("release-notes.sh %s against the real CHANGELOG: %v", ver, err)
	}
	if len(strings.TrimSpace(got)) < 20 {
		t.Errorf("release body for %s is trivially short (%d bytes)", ver, len(strings.TrimSpace(got)))
	}
}
