#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Print the CHANGELOG.md section for a release tag, for use as the GitHub release
# body (goreleaser --release-notes).
#
# The goreleaser config sets `changelog: disable: true` (the hand-written
# CHANGELOG is the source of truth, not git log). Without notes supplied
# explicitly that produced an EMPTY release body on v0.5.0 and v1.1.0-v1.1.3.
# This script makes CHANGELOG.md that explicit source, and exits non-zero when
# the section is missing so a release fails loudly instead of publishing empty.
#
# Usage: scripts/release-notes.sh v1.1.3 [CHANGELOG.md]
set -euo pipefail

tag="${1:?usage: release-notes.sh <tag> [changelog]}"
changelog="${2:-CHANGELOG.md}"

[ -r "$changelog" ] || { echo "release-notes: cannot read $changelog" >&2; exit 1; }

ver="${tag#v}"

# A prerelease (v1.2.0-rc1) usually has no section of its own — it is cut from the
# same CHANGELOG state as the release it rehearses — so fall back to the base
# version (1.2.0) before giving up.
section() {
	awk -v want="## [$1]" '
		index($0, want) == 1 { found = 1; next }
		found && /^## \[/    { exit }
		found                { print }
	' "$changelog"
}

notes="$(section "$ver")"
if [ -z "${notes//[[:space:]]/}" ] && [ "$ver" != "${ver%%-*}" ]; then
	notes="$(section "${ver%%-*}")"
fi

if [ -z "${notes//[[:space:]]/}" ]; then
	echo "release-notes: no CHANGELOG section for [$ver] in $changelog." >&2
	echo "release-notes: add a '## [$ver] - <date>' section before tagging." >&2
	exit 1
fi

# Trim leading/trailing blank lines.
printf '%s\n' "$notes" | awk 'NF {found = 1} found {print}' | awk '
	{ lines[NR] = $0; if (NF) last = NR }
	END { for (i = 1; i <= last; i++) print lines[i] }
'
