# Contributing to lith

Thanks for your interest in lith. A few rules keep the project coherent.

## Issue first

Every change starts as a GitHub issue. Planning, roadmap, and design live in
GitHub Issues, Labels, Milestones, and the project board — not in tracked
markdown files. The design document itself is the pinned "Design" issue. Open
or find an issue before you start; reference it in your commits and PR.

## Conventional commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add S3 Inventory manifest index source
fix: correct readdir cursor at arena boundary
chore: bump aws-sdk-go-v2
docs: document --requester-pays
test: cover zero-length folder keys
ci: build linux/arm64
perf: coalesce adjacent block misses
```

Reference issues in the body or footer: `Refs #12`, `Closes #12`.

## Changelog

Every user-visible change gets a line under `[Unreleased]` in `CHANGELOG.md`,
using the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) headings
(`Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`). The PR
template has a checkbox for this.

## Versioning

[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). Releases are
tagged `vX.Y.Z`. The index file format is lith-private and versioned, with a 1.x
compatibility promise: every lith 1.x reader reads every index produced by lith
1.0.x and `pkg/lithindex` 1.x (see [docs/scope.md](docs/scope.md); golden
fixtures under `internal/index/testdata/golden` enforce it).

## Before you push

- `make lint` (`go vet` + `golangci-lint`) is clean.
- `make test` (race-enabled) passes; new code has tests and touches no network.
- Every `.go` file carries the `// SPDX-License-Identifier: Apache-2.0` header.

## Quoting a performance number

This project's findings are mostly measurements, and the measurements get quoted — into
`CHANGELOG.md`, into issues, and back to the deployments that reported them. Three rules, each
of which exists because its absence cost a wrong conclusion that had already been published.

**Model the cost with its denominator.** State the full expression, including what it divides
by, and where each term came from. The error lives in the term that seems obvious: a 1 MiB
in-region S3 GET costs **~28 ms** to first byte, not the 2.2 ms network round trip — that one
mistake made a 225 ms cost look like 15 ms and got an issue closed. A whole-object read's
baseline is `intercept + size/rate`, not `size/rate`. And `grep` for bounds before modelling a
path: two models have been refuted by a clamp already present in the function being modelled
(`EOF` and dedup in `Prefetch`, and the `cursor+1` frontier clamp).

**Never quote one draw.** Repeat the run — six is enough to see variance — and measure the
**before** state under the same conditions, by temporarily reverting the change rather than
trusting a figure from an earlier session or a different fixture. A GET count published as
`22 → 16` was `22 → 17` on six repeats. Report the stable value or the range.

**Assert volume, not presence.** A test that checks a state, a non-zero, or a loose bound
cannot catch a regression of the thing it was written for: `TestColdSequentialGetShape` once
allowed "≤ 4× the ideal" and would not have noticed its own fix being reverted. Assert the
number, tightly enough that reverting fails, with a floor so a fixture serving everything from
cache fails instead of passing quietly. And have the fixture state its own precondition —
several tests here have passed while proving nothing, by skipping silently, by reaching a
different detector state than intended, or by using a 1 MiB-block store to measure an
8 MiB-block effect.

A number that changes a default or goes into a release note is worth re-checking on `main`
after the merge. That is one command, and it is what caught the `16` above.

## License

By contributing you agree that your contributions are licensed under the
Apache License 2.0.
