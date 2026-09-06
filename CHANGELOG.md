# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Index file format bumped to **v2**: the directory table now stores a
  per-directory inode and mtime. v1 index files are rejected on load with a
  message to rebuild.

### Fixed

- Directory and file inodes now share a single build-time collision namespace,
  so a directory inode can no longer silently collide with a file inode;
  colliding directory inodes take the sequential fallback.
- Directory mtime is the maximum `LastModified` over the directory's entire
  subtree (falling back to the index build time when no descendant is dated),
  replacing the previous zero value.

### Added

- Repository scaffold: Go module `github.com/scttfrdmn/lith`, internal package
  layout (`index`, `s3client`, `blockstore`, `prefetch`, `fuse`, `version`),
  and a `Makefile` with `build`, `test`, `lint`, `bench`, and `cover` targets.
- `lith version` command printing the version, commit, and build date, injected
  at build time via `-ldflags`.
- CLI skeleton (`cobra`): `mount`, `index`, and `bench` subcommands are present
  and report "not implemented in this build" pending M1/M2.
- Continuous integration (`go vet`, `golangci-lint`, `go test -race`, and
  `linux/amd64` + `linux/arm64` builds) and a tag-triggered goreleaser release
  workflow producing static binaries and a checksums file.
- Project documentation: `README`, `CONTRIBUTING`, `SECURITY`,
  `CODE_OF_CONDUCT`, issue templates, and a pull-request template.
- Dependabot configuration for Go modules and GitHub Actions.
- Namespace index: an immutable, sorted-arena snapshot of a bucket with local
  `Lookup`, `Stat`, and constant-memory paginated `Readdir`; directory
  structure is derived from sorted keys rather than stored. Includes POSIX key
  sanitization, directory-shadows-object resolution, xxh3 inode assignment with
  collision fallback, and a versioned, memory-mappable on-disk format.
- Index builders: from a paginated `ListObjectsV2` pass (with optional
  concurrent prefix sharding) and from an S3 Inventory manifest (CSV).
- Tuned aws-sdk-go-v2 S3 client: region resolved from the bucket, HTTP/1.1 with
  a large keep-alive connection pool, and `--no-sign-request`,
  `--requester-pays`, `--endpoint`, and `--path-style` options.
- `lith index build|refresh|inspect` commands.
- In-process fake S3 (ListObjectsV2/HeadObject/GetObject with Range) backing all
  unit tests, which run with the race detector and touch no network.

[Unreleased]: https://github.com/scttfrdmn/lith/commits/main
