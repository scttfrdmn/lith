# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- The block cache is now **chunk-granular**: a fixed 1 MiB chunk is the cache
  unit, and `--block-size` (default 8 MiB) is the fill/readahead unit (a run of
  chunks coalesced into one range GET). The on-disk cache layout is versioned
  (`chunkv1`); caches written by earlier builds are ignored, not misread.
- `lith bench` now reads through an actual lith mount via `pread`, so the FUSE
  per-handle prefetcher is exercised (it previously read the block store
  directly and never prefetched).
- `--mem-cache` now defaults to **25% of system memory** (from `/proc/meminfo`,
  1 GiB fallback), replacing the fixed 1 GiB default; `--disk-cache` stays off
  by default. `lith mount` warns if the `--disk-cache` path resolves onto the
  root filesystem or a network volume. README gains a "No NVMe?" section.
- `lith bench` gains `--runs N` (N cold runs; min/median/max reported, then a
  warm run), `--readers N --objects k1,k2,...` (concurrent multi-object
  readers; aggregate MB/s and S3 request count), per-read latency percentiles
  split at the first readahead window, time-to-first-byte, and `--cache-dir`.
- Default `--s3-concurrency` raised to **128** and `--max-readahead` to **64**
  (from 64 and 32). On a c8gd.4xlarge in-region against `s3://1000genomes`,
  128/64 won on both cold sequential (1248 MB/s vs 1126 at 64/32) and cold
  stride, so it wins on both workloads the tuning grid measured.
- Index file format bumped to **v2**: the directory table now stores a
  per-directory inode and mtime. v1 index files are rejected on load with a
  message to rebuild.

### Changed

- The FUSE mount now negotiates 1 MiB requests (`max_pages` = 256 via
  `MaxWrite`), so the kernel issues one read per 1 MiB chunk (~1024 read
  ops/GiB instead of ~8192) and reads stay on the zero-copy path.
- A read contained within one chunk returns a sub-slice of the (immutable,
  cached) chunk buffer directly to the kernel — no allocation and no copy on a
  cache hit. Boundary-straddling reads still assemble and are counted in
  `lith_read_straddle_total` (≈0 for aligned sequential reads).
- The memory tier is **sharded** into 64 independently-locked 2Q shards, keyed
  by chunk hash. Under concurrent readers this removes the single memory-tier
  mutex as a serialization point (profiling showed ~all mutex delay there,
  held during an O(n) eviction scan).
- New `--inflight-bytes` budget bounds total bytes in flight to S3 (default
  2 × NIC bandwidth × 100 ms, detected via `ethtool`; 512 MiB fallback).
  `--s3-concurrency` remains a request-count hard cap.
- The disk cache tier is now **write-behind**: a fill completes its readers as
  soon as the bytes are in the memory tier, and the disk write is handed to a
  bounded pool (`--disk-writers`, default 4) off the fill path. Memory chunks
  with a pending disk write are pinned so they are not evicted before the write
  lands. The metrics endpoint (`--metrics`) also serves Go `pprof`.

### Fixed

- Prefetch now dispatches the readahead window **ahead of the demand cursor** —
  on `open` (initial 2-block window) and on every window advance — instead of
  reactively on the read that reveals a gap. A demand read that finds its chunk
  neither cached nor in flight is counted as `lith_prefetch_uncovered_total`.
- Cold read throughput: a single chunk singleflight keyed by
  `(key, etagHash, chunkIdx)` with in-flight join replaces the range-keyed
  singleflight. A demand read for a chunk a prefetch is already fetching now
  joins that fetch (completing as the chunk's bytes arrive) instead of issuing a
  duplicate GET — eliminating the ~11× GET amplification observed in session 2.
- Random 4 KiB reads now fetch a single 1 MiB chunk instead of a whole 8 MiB
  block.
- Directory and file inodes now share a single build-time collision namespace,
  so a directory inode can no longer silently collide with a file inode;
  colliding directory inodes take the sequential fallback.
- Directory mtime is the maximum `LastModified` over the directory's entire
  subtree (falling back to the index build time when no descendant is dated),
  replacing the previous zero value.

### Added

- `lith mount s3://bucket[/prefix] /mnt/point`: a read-only FUSE mount served
  from the local index and a tiered block cache. Flags include `--index-file`
  (auto-built under `--auto-index-limit` when absent), `--mem-cache`,
  `--disk-cache`/`--disk-path`, `--block-size`, `--max-range`,
  `--s3-concurrency`, `--max-readahead`, `--small-file`, `--metrics`,
  `--allow-other`, `--uid`/`--gid`, `--exec`, and `--daemon`. Mutating
  operations return `EROFS`; clean unmount on SIGINT/SIGTERM.
- `lith bench s3://bucket/key --pattern seq|rand4k|stride [--against PATH]`:
  measures MB/s, IOPS, p50/p99, S3 requests and bytes, and an estimated cost,
  cold then warm, optionally alongside another mount (e.g. mountpoint-s3).
- Read path: a 2Q memory + NVMe disk block cache with range coalescing,
  singleflight, a demand-favoring worker pool, a per-handle prefetcher
  (sequential/strided/random), and ETag-mismatch detection (`EIO`).
- Prometheus metrics endpoint (`--metrics`): cache hits by tier, S3 bytes and
  requests, in-flight gauge, prefetch accuracy, stale objects, and FUSE op
  latency histograms.
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
