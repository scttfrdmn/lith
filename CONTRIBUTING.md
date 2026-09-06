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
tagged `vX.Y.Z`. The index file format is lith-private and versioned; it carries
no compatibility promise before v1.

## Before you push

- `make lint` (`go vet` + `golangci-lint`) is clean.
- `make test` (race-enabled) passes; new code has tests and touches no network.
- Every `.go` file carries the `// SPDX-License-Identifier: Apache-2.0` header.

## License

By contributing you agree that your contributions are licensed under the
Apache License 2.0.
