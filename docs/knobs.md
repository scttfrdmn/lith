# Knobs

Every flag, grouped by the trade it makes. Defaults are chosen to be right for
most workloads; each line says what the default is and why. The canonical,
never-drifting list is always `lith mount --help`, `lith index build --help`,
and `lith bench --help` — this page groups those and explains them.

## Bytes vs latency

How much lith fetches per miss. Bigger reads amortize round-trips but waste
bandwidth on bytes you won't use.

| flag | default | why |
|---|---|---|
| `--block-size` | `8MiB` | The fill/readahead unit — a run of 1 MiB cache chunks coalesced into one range GET. 8 MiB balances round-trip amortization against over-fetch. |
| `--max-range` | `64MiB` | Caps how many contiguous blocks coalesce into a single GET, so one fill can't monopolize a connection. |
| `--small-file` | `4MiB` | Files at or below this are fetched whole on first read — one GET beats seeking within a tiny object. |
| `--parts-max` | `auto` | Files up to this are fetched whole as **concurrent block-sized range parts** — but only **once the access pattern proves it tiles** ([#229](https://github.com/scttfrdmn/lith/issues/229)): a sequential reader establishes within the first couple of reads and then gets the whole-file parts fetch (what lets a 31 MB granule beat `aws s3 cp`); a reader that only wants a slice (a hyperslab, a footer probe) never establishes and is served byte-exact instead of pulling the whole object. The size threshold is `auto` — derived from NIC baseline × first-byte latency, clamped to `[--small-file, 64MiB]` ([#220](https://github.com/scttfrdmn/lith/issues/220)): a fat pipe would fetch whole cheaply, a thin pipe keeps a sub-file read byte-exact. An explicit size overrides; `0` disables. |
| `--bgzf-whole-file-max` | `512MiB` | For a bgzf-family data file (BAM/CRAM/VCF.gz/BCF) that has a coordinate-index sibling (`.bai`/`.tbi`/`.csi`/`.crai`), files at or below this are fetched whole through the parts path when the index opens — a region-indexed scan (e.g. `tabix` over many regions) touches nearly every block, so the whole-file working set makes cold ≈ warm (v0.3, [#107](https://github.com/scttfrdmn/lith/issues/107)). Above it, tier-2 prefetches only the index-resolved slices/chunks. `0` disables whole-file prefetch. The handle keeps its adaptive readahead window either way — the plan only *adds* ranges. |

**Cold-start cost of "prove it first" ([#229](https://github.com/scttfrdmn/lith/issues/229)).** lith will not fetch
broadly (whole-file parts, or wide readahead) until a handle's reads show they tile — the first couple of reads are
served precise. This is what keeps a hyperslab or a footer probe from pulling a whole object. The cost lands on the
opposite case: a **cold sequential read of a mid-size file** (a `cat`/`cp` off the mount) pays roughly one extra
round-trip of latency for its first block, because that block is demand-fetched in chunks before the pattern
establishes and the whole-file fetch kicks in. It is cold-only (a warm re-read is unaffected), byte-identical, and
shrinks with file size (the first block is a smaller fraction of a larger file). The block-0 coalescing gap that
would erase even this is tracked in [#233](https://github.com/scttfrdmn/lith/issues/233).

## RAM vs re-fetch

How much lith keeps in memory (and optionally on disk) so a re-read is free
instead of another GET.

| flag | default | why |
|---|---|---|
| `--mem-cache` | 25 % of system RAM | The in-memory tier. 25 % leaves room for the app and the page cache; raise it for re-read-heavy work with spare RAM. **Per-daemon:** the default is 25 % of RAM *for each mount*, so several mounts on one host add up — five mounts default to a 125 % cap. If you run more than one mount on a box, set `--mem-cache` explicitly so they sum to a sane fraction ([#242](https://github.com/scttfrdmn/lith/issues/242)). **Values below one 1 MiB chunk disable the tier entirely** and the mount warns; between one chunk and 64 MiB the tier now scales its shard count to stay usable, where it used to accept nothing ([#307](https://github.com/scttfrdmn/lith/issues/307)). **Size it at ~2.5× your distinct working set, not 1×** — see below. **It does not bound RSS:** the footprint is the tier *plus* outstanding prefetch, measured additive to 0.2% — a 20 GB tier on a 33 GB box reached 30.56 GB RSS and was OOM-killed ([#314](https://github.com/scttfrdmn/lith/issues/314)). |
| `--prefetch-budget` | 50 % of `--mem-cache` | Bytes prefetch may hold un-demanded. Bounding it to half the tier stopped concurrent readers thrashing a small cache ([#55](https://github.com/scttfrdmn/lith/issues/55)). |
| `--disk-cache` | `0` (off) | An on-disk second tier. Worth it only on **fast local NVMe** for working sets larger than RAM that you re-read; never on EBS/EFS/NFS. |
| `--disk-path` | `$TMPDIR/lith-cache` | Where the disk tier lives. Point it at your instance-store mount or `/dev/shm`. |
| `--disk-writers` | `4` | Write-behind workers that persist chunks off the read path, so disk writes never stall a reader. |

!!! warning "`--mem-cache` is the tier, not the process footprint"

    In-flight prefetch is not charged against the cache it is about to land in, so resident memory is the tier **plus** the outstanding burst. Measured additive to 0.2%: a mount asked for a 20 GB tier on a 33.0 GB box and the kernel killed it at 30.56 GB RSS (20 + 13.078 = 33.08 GB against a 33.02 GB `MemTotal`). At stock defaults on that same box the footprint is 8.256 + 13.078 = **21.3 GB** for a flag that reads 8.256.

    The burst is bounded by `concurrent readers × --max-readahead × --block-size`, and because `--max-readahead` derives from the NIC while the tier derives from RAM, it grows with the link rather than with the memory available to hold it. Budget for both, or lower `--max-readahead` ([#314](https://github.com/scttfrdmn/lith/issues/314), [#313](https://github.com/scttfrdmn/lith/issues/313)).

**Sizing `--mem-cache`: budget ~2.5× your distinct working set, not 1×.** The memory tier is
**64 independently-evicting shards**, with a chunk assigned by a hash of its key — so capacity
is divided 64 ways and a shard evicts while its neighbours sit idle. Hash skew means the
busiest shard holds well above the mean, and a cache that "just fits" by total bytes will
re-fetch anyway. Measured by replaying a real workload's trace through the production tier
(3.4 GiB distinct across 204 objects):

| `--mem-cache` | headroom vs working set | re-fetched |
|---|---|---|
| 24–32 GB | 6.7× | **nothing** — eviction never fires |
| 8 GB | 2.2× | 5.0 MiB |
| 6 GB | 1.7× | 48.9 MiB |
| 4 GB | 1.1× | **2.9 GiB** (plus 251 prefetched chunks discarded unread) |
| 2 GB | 0.6× | 16.5 GiB — more than double the run's entire S3 traffic |

So the cliff is not at 1.0× but a little above 2×, and it is steep: between 1.7× and 1.1× the
re-fetch cost rises by 60×. A pooled cache would hold this working set at 1.0×; the ~2.2×
is what sharding costs. If you are tuning against a bill, measure the distinct bytes your job
touches and multiply by 2.5.

## Prefetch depth vs burst credits

How aggressively lith reads ahead. Deeper prefetch fills a fat NIC but can
drain a burst-credit box's credits faster than it helps.

| flag | default | why |
|---|---|---|
| `--max-readahead` | 1.5 × the bandwidth-delay product | Sequential readahead window in blocks. The 1.5× is **empirical**, measured on a `c8gd.16xlarge` ([#56](https://github.com/scttfrdmn/lith/issues/56)); a raw-BDP window left the NIC underfed. |
| `--inflight-bytes` | 2 × NIC baseline × 100 ms | Total bytes in flight to S3. NIC bandwidth is detected `ethtool` → EC2 `DescribeInstanceTypes` baseline → a fixed fallback, so burst-credit classes like `c8g` are now sized from their baseline automatically ([#79](https://github.com/scttfrdmn/lith/issues/79)); the result is cached in `nic.json` next to the index. |
| `--nic-gbps` | detect | Override the detected NIC bandwidth (Gbps) — it sizes `--inflight-bytes`, the readahead window, **and the device-derived `--parts-max`/`--coalesce-gap`**. Detection tries `ethtool` → cache → `ec2:DescribeInstanceTypes` → an IMDS size estimate → a 10 Gbps fallback. **On AWS ParallelCluster and other least-privilege HPC roles, pass this explicitly:** ENA reports no `ethtool` speed and the generated node role omits `ec2:DescribeInstanceTypes`, so lith falls to the IMDS size estimate (approximate) — `--nic-gbps` gives the exact baseline, and `lith doctor` WARNs when the NIC is undetected ([#79](https://github.com/scttfrdmn/lith/issues/79), [#237](https://github.com/scttfrdmn/lith/issues/237)). **Pass the sustained baseline, not the "Up to N Gigabit" figure on the instance page — that is the *peak*.** Overstating it (e.g. `15` for a c7g.4xlarge whose baseline is `7.5`) over-sizes the readahead window and fetches bytes that are never read, for no speed gain; `doctor` WARNs when `--nic-gbps` looks like the advertised peak ([#239](https://github.com/scttfrdmn/lith/issues/239)). |
| `--s3-concurrency` | `128` | Hard cap on concurrent S3 requests. 128 won on both cold sequential and stride in the tuning grid ([#40](https://github.com/scttfrdmn/lith/issues/40)). |
| `--prefetch-concurrency` | `--s3-concurrency` | Sub-cap on concurrent prefetch fills; lower it to reserve request slots for demand reads under heavy prefetch. |
| `--sibling-readahead` | `16` | On a directory walked in key order, prefetch this many following small siblings whole (v0.2, [#63](https://github.com/scttfrdmn/lith/issues/63)) — the chunked-store path. `0` disables. |
| `--sibling-window` | `4` | How close in index order two successive opens must be to count as "walking" the directory. |

## Index

The namespace snapshot that makes metadata free.

The `s3://bucket/prefix` you pass to `mount` is the **root** of the filesystem:
the mount shows only what lives under that prefix, with the prefix stripped from
every path. An index whose own build root is that prefix — **or any parent of
it** — can serve the mount, so one wide index backs many prefix mounts
concurrently, with no rebuild. A prefix the index cannot cover (narrower than,
or disjoint from, its root), or one with no keys under it, is rejected rather
than mounted empty. `lith index inspect` prints the index's `root:`.

| flag | default | why |
|---|---|---|
| `--index-file` | (auto-build) | Load a prebuilt index. Without it, `lith mount` auto-builds from the bucket, bounded by `--auto-index-limit`. Prebuild for large buckets and reuse across nodes — one whole-bucket or parent-prefix index can back many prefix mounts at once. |
| `--auto-index-limit` | `5000000` | Max keys `lith mount` will auto-index; above this, build explicitly with `lith index build` first. |
| `lith index build --inventory` | — | Build from an S3 Inventory manifest instead of a `ListObjectsV2` pass — far cheaper for very large buckets. |
| `lith index build --shard` | — | List sub-prefixes concurrently (repeatable) to speed a build over a wide namespace. |
| `lith index build --page-size` | `1000` | `ListObjectsV2` page size. |
| `lith index refresh` | — | Re-list the bucket and rewrite the index when keys have changed (lith never mutates the bucket; the index is a private cache). |
| `lith index build --keys` | — | Build from an explicit key list (one key per line; optional `\t<size>\t<mtime>`), for buckets that are GET-public but **deny `ListObjectsV2`** (Common Crawl `cc-index`, `nyc-tlc`). Keys lacking size/mtime are `HeadObject`-ed. |
| `lith index build --keys-from-manifest` | — | Same, but the key list is fetched from an `s3://` URL or local path (`.gz` transparent). |
| `lith index build --keys-allow-missing` | off | Skip keys that `HeadObject` reports 403/404 instead of failing the build. |

## Access

Authentication, endpoint, and how files present to the OS.

| flag | default | why |
|---|---|---|
| `--no-sign-request` | off | Anonymous requests, for public buckets (RODA). |
| `--requester-pays` | off | Add the requester-pays header when the bucket requires it. |
| `--endpoint` | (AWS) | Point at an S3-compatible store. |
| `--path-style` | off | Path-style addressing for stores that need it. |
| `--region` | (resolved) | Resolved from the bucket if empty. |
| `--allow-other` | off | Let other users access the mount (needs `user_allow_other` in `/etc/fuse.conf`). **Security:** files are world-readable (`0444`/`0555`) with no per-object access control, so this exposes the entire mounted subtree to every local user regardless of the bucket's S3 ACLs — enable it only where that is acceptable. |
| `--uid` / `--gid` | your uid/gid | Owner reported for every file. |
| `--exec` | off | Report files as mode `0555` instead of `0444` (executables on the mount). |

## Observability

| flag | default | why |
|---|---|---|
| `--metrics` | off | Serve **Prometheus metrics only** (`/metrics`) on an address (e.g. `:9101`): cache hits by tier, S3 bytes/requests, prefetch accuracy, uncovered misses, FUSE op latency, and the **realized readahead window with its divisor** (see below). No pprof (see `--pprof`). |
| `--pprof` | off | Serve Go `net/http/pprof` handlers on an address (e.g. `127.0.0.1:6060`). **Security:** pprof exposes the process argv (`/cmdline`) and an on-demand CPU/goroutine profiling DoS (`/profile`, `/trace`) with no auth — **bind it to localhost and never expose it to an untrusted network.** Enabling it also turns on block/mutex profiling. |
| `--pf-trace` | off | **Diagnostic ([#262](https://github.com/scttfrdmn/lith/issues/262)).** One CSV row per read recording what the access-pattern detector saw and decided: `fh,pid,key,off,len,blk,gap,path,state_before,state_after,window,dispatched,peak_window`, preceded by a `#` header line with the config that produced it (block size, max-readahead, parts-max, small-file, coverage gate, evidence ratio) so traces are self-describing and comparable across runs. **Group by `fh`:** lith builds one prefetcher per `open`, so the handle — not the key — is the unit that makes decisions, and concurrent handles on one object otherwise interleave indistinguishably. **`path` marks which read path served the row** (`window` = the prefetcher was driven; `parts` = a whole-file parts fetch was already in flight; `footer` = a byte-exact projection plan replaced the window), so reads the detector never saw are marked rather than silently dropped — they are a large, non-random share of the bytes on a mount where [#229](https://github.com/scttfrdmn/lith/issues/229) whole-fetches most large objects. `LITH_PF_TRACE` is the older env-var spelling; the flag wins. **Unbounded, and serialized under one mutex** — but measured at **+0.5–1.6% wall** on a 48-rank MPI job over ~60k traced reads, so the lock is negligible below roughly 10^5 reads/run and the traced run describes an untraced one. Budget for the file instead: one row per read (~3.5–4.1 MB for 60k rows). |
| `--timeline-csv` | off | Diagnostic ([#70](https://github.com/scttfrdmn/lith/issues/70)/[#95](https://github.com/scttfrdmn/lith/issues/95)): write a per-chunk demand-read timeline — join-wait, in-flight fill depth, and prefetch-dispatch→open lag — to this CSV on unmount. Opt-in; no effect on the read path when unset. |

**`--max-readahead` is an upper bound, not the window — watch `lith_readahead_window_blocks`.** `--prefetch-budget / --block-size` caps it too, and the mount warns at startup when it does ([#297](https://github.com/scttfrdmn/lith/issues/297)).

It is **also** capped by how many readers are streaming at once: the depth is `clamp(--prefetch-budget/--block-size / streaming-handles, 2, --max-readahead)`, where `streaming-handles` counts the handles the detector is actually prefetching for.

**That divisor used to count open file descriptors, and that was [#301](https://github.com/scttfrdmn/lith/issues/301).** Per descriptor, across processes, including descriptors never read — so a job holding 256 files open drove its own and every other reader's readahead to the floor of **2 blocks** whatever the flag said, at **6–10× the wall clock** on bytes and requests differing by 0.4% and 3%. No byte or request counter could see it. Both production mounts of the workload that found it were running there, by way of 48 ranks × ~6 files they did not account for. Fixed by changing what the divisor counts: idle descriptors now contribute nothing.

**The division itself is deliberate, and removing it was tried and reverted.** Enforcing `--prefetch-budget` byte-exactly on the measured total instead — which bounds the same aggregate, to within 2.4% — made concurrent readers **5.66× slower**. A share is an *allocation discipline*, not just a ceiling: 16 × 30 blocks covers sixteen readers shallowly, while 2 × 223 + 14 × 0 commits the same total and covers two, and first-come admission produces the second. Removing the rationing outright is worse again: 13.8–22.4× wall and 4.2–5.2× the bytes, with 81–92% of prefetch evicted unread.

**The obvious fix was tried and reverted, which is worth knowing before you propose it.** Admitting prefetch byte-exactly against `--prefetch-budget` where the quantity is measured removes the over-charge cleanly in isolation — 2.8× faster at 64 held descriptors and 4.5× at 256, identical bytes and identical GETs — and it bounds the same aggregate to within 2.4% of what the divisor realizes. On **concurrent readers** it was 5.66× *slower*. The division is not just a total, it is an allocation discipline: 16 × 30 blocks covers sixteen readers shallowly, while 2 × 223 + 14 × 0 commits the same total and covers two, and byte-exact first-come admission produces the second. So the repair changed what the divisor **counts**, not whether it divides — measured at **9.26×** on the shape that found it (255 never-read descriptors took a streamer from 25.74 s to 2.78 s).

**The mount logs all three bounds on outstanding prefetch with the binding one named** ([#298](https://github.com/scttfrdmn/lith/issues/298)). One handle's window commitment (`--max-readahead × --block-size`), the mount-wide `--prefetch-budget`, and `--inflight-bytes` are derived from three unrelated quantities — an empirical multiple of the bandwidth-delay product, a fraction of RAM, and NIC × latency — and in the shipping default they disagree by **1.5×**, with the smallest winning. That is why raising `--max-readahead` from 223 to 492 once measured **+3%**: both configurations were already against a ceiling neither of them set. Check the `binding` field before tuning any of them.

`lith_readahead_window_blocks` reports the realized depth and `lith_open_handles` the divisor; you need both, because a window of 2 could be a tight budget or a crowded mount and only the divisor distinguishes them.

**The mount now tells you where the floor is.** The `prefetch bounds` line reports `full_window_descriptors` — how many concurrent open descriptors can each hold the whole window — and `floor_at_descriptors`, the count past which everyone is at 2 blocks. At the shipping default on a 33 GB box those are **2** and **165**, so a job with 288 descriptors open is well past it. And because that count includes descriptors other processes hold, which no startup line can predict, a `WARN` is also emitted the first time a share actually reaches the floor. If you see it, `--prefetch-budget` is the lever: it is the numerator of every reader's share.

**Diagnosing a concurrent-start thrash: read `lith_prefetch_unread_resident_bytes` against `--mem-cache`.** `lith_prefetch_committed_bytes` is charged at *dispatch*, before the prefetch and S3 semaphores, so it counts resident-unread **plus queued-for-a-slot plus on-the-wire** — and `--inflight-bytes` bounds only the last of those. At rest the two gauges are equal to the byte; in flight they differ by however much prefetch is queued or on the wire, which on one measured box reached 26.9 GB. Only the resident part can evict anything, so the collapse condition is resident-unread approaching the tier's **capacity**: at that point every arriving chunk must evict an unread one, there being nothing else left to take. Measured, that separates clean runs (≤ 0.84 of tier) from collapsed ones (≥ 0.9996) with a margin of 0.16, where the committed ratio separates them by 0.002 ([#313](https://github.com/scttfrdmn/lith/issues/313), [#320](https://github.com/scttfrdmn/lith/issues/320)).

**The window is also only a *proxy* for what `--prefetch-budget` bounds.** The budget is about bytes prefetched and still resident unread; the per-handle window times the handle count is a stand-in for it. `lith_prefetch_committed_bytes` is the real quantity and `lith_prefetch_budget_bytes` its limit — **measured but not enforced**, because the thing that enforced them byte-exactly is the change that was reverted. Against the old open-descriptor divisor the proxy over-charged by exactly that count (tightness `1/N`, matched to within 6.2% from N=1 to N=256): at 256 descriptors a mount was charged 4295 MB, held **17.8 MB**, and was throttled ~9.5× for it. Two cautions when reading these. Do **not** compare committed bytes against window × handles: an estimator built that way returns the standing window by construction, so the comparison is an identity rather than a check. And committed bytes are **not** released when a handle closes, so on a mount whose working set fits the memory tier the figure ratchets upward — which is the second reason byte-exact enforcement could not ship ([#301](https://github.com/scttfrdmn/lith/issues/301)).

`committed`, not resident: the counter increments at **dispatch**, so it includes bytes whose GET is still in flight. That is the right quantity for admission control and the wrong one for a memory limit. The mount also warns at startup when the configured depth is already unreachable ([#297](https://github.com/scttfrdmn/lith/issues/297)) — but that check is single-handle, so it cannot see the divisor, which is dynamic.

**Reading `lith_prefetch_used_total / lith_prefetch_issued_total`.** It is a **chunk-touch** rate, not a byte rate: a 64 KiB demand read marks the whole 1 MiB chunk *used*. So the ratio **overstates byte follow-through, and overstates it most for the low-coverage readers where prefetch is least useful** — a measured case reported 89% by this ratio while at most ~25% of the prefetched bytes were ever read ([#256](https://github.com/scttfrdmn/lith/issues/256)). Use it to compare like with like, not as an efficiency.

**And `lith_prefetch_issued_total` is blind to the failure it looks relevant to.** Three independent measurements have now found it *identical* across arms whose wall clock differs 3×: in one, 57045 issues in all eight cells. Evicted blocks come back as **uncovered demand reads**, not as new prefetch issues, so the counter that moves is `lith_prefetch_uncovered_total` — 256 in the fast arms against ~5150 in the slow ones — alongside `lith_prefetch_evicted_unread_total`. If you are diagnosing a thrash, read those two and ignore `issued` ([#313](https://github.com/scttfrdmn/lith/issues/313)).

**`mmap` with a random access pattern is the one shape lith cannot help, and the cost is round trips rather than bytes.** A program that `mmap`s an object and touches scattered pages gets one synchronous page fault per miss, and a single-threaded fault stream is serial by construction — there is nothing for lith to overlap. Measured on a real mount: 3000 random 4 KiB touches over an 892 MB object took **326 s**, which is ~109 ms per fault, or one round trip each ([#232](https://github.com/scttfrdmn/lith/issues/232)).

The bytes are a separate and smaller story, and most of them are not lith's choice. Measured offline, lith is faithful to the read it is handed — **0.8×** the requested bytes at the kernel's own 128 KiB readahead size — but its floor is one **64 KiB extent**, so a 4 KiB read costs 64 KiB (**15.3×**) and a 4 KiB *touch* costs whatever the kernel decided to ask for. On the reported run that was ~220 KiB per touch, i.e. ~55× against what the program wanted, of which only ~3.4× was lith's extent rounding. Reducing lith's granularity would therefore address a small fraction of the bytes and none of the wall clock. If you have this shape, `O_DIRECT` bypasses the page cache and was measured restoring normal behaviour on a related issue ([#316](https://github.com/scttfrdmn/lith/issues/316)).

**A handle that never prefetches is not necessarily a scattered walk.** Check `lith_prefetch_low_coverage_total`. Concurrent readers of **one** object have each other's reads served from the shared kernel page cache and never reach lith, so each handle sees a punctate offset stream, the coverage gate classifies it as a walk, and nothing is prefetched. Measured at **243× slower than a single reader** with byte amplification of 1.001 — the cleanest byte count in the whole gate ([#316](https://github.com/scttfrdmn/lith/issues/316)). If that counter is climbing while `lith_streaming_handles` sits at zero and many readers share a file, this is the shape you are in.

**"Low coverage", not "scattered" — a correction.** Through most of [#256](https://github.com/scttfrdmn/lith/issues/256) this page and lith's own comments described the losing case as *small scattered reads*. A key-level fit over two mounts of one workload showed that was reading the wrong column. The two mounts had **the same read size** (104 vs 122 KiB median), both were **sub-chunk on every single read**, and both walked their objects **monotonically end to end** — so `sequential` was the *correct* classification and the detector was not fooled. What separated them was how much of each object the workload ever touched: **16% against 65%**. Coverage — not read size, not read order, not reader count, not interleaving — is what predicts the over-fetch (ρ −0.69 on both arms, against a permutation null centred at +0.17). The practical consequence: when judging whether a reader will over-fetch, measure `union(bytes read) / object size`, not the read size. For a byte-level answer, `lith_fill_bytes_total{kind="demand"|"whole"}` splits demand fetches from readahead exactly, and `--pf-trace` plus an offline replay gives per-handle byte follow-through.

## Experimental

Off by default; enable only if you have measured a win on your own workload.

| flag | default | why |
|---|---|---|
| `--footer-tier2` | `false` | **Experimental, clustered projections only.** Byte-precise projection fetch for footer-family containers (Parquet/ORC/Arrow/zip) — a touched Parquet row group's projected column chunks, or a read zip entry + the next few in directory order ([#108](https://github.com/scttfrdmn/lith/issues/108)). Measured (sessions 29–43): it **wins on bytes for a *clustered* projection** (adjacent columns — ~10× fewer bytes than whole-file streaming) but **loses on wall-clock for a *spread* projection**, where the achievable floor is the reader's own footprint (pyarrow fetches ~the same bytes) and a whole-file stream is faster on a fat pipe (a handful of GETs at line rate vs a round-trip per column region). So it is off by default and stays experimental. Leave off unless you have a clustered projection where bytes-saved is the goal. Tier 1 (footer + head prefetch on open) always runs regardless. |
| `--prefetch-coverage-min` | `0` (use the characterized `0.5`) | **Experimental ([#316](https://github.com/scttfrdmn/lith/issues/316)).** The [#221](https://github.com/scttfrdmn/lith/issues/221) coverage threshold: a handle establishes readahead only once its trailing 16 reads cover at least this fraction of their own byte span, and below it the handle is held provisional and prefetches **nothing**. The default comes from one characterization (streams ≥ 0.89, scattered walks ≤ 0.07) and does **not** account for concurrent readers of one object — see below. |
| `--readahead-evidence-ratio` | `0` — **decide from measured latency** | Bound a committed readahead window to this multiple of the bytes the handle has actually read, so a single reader that wants a slice of a large object stops prefetching the whole thing. **Since [#284](https://github.com/scttfrdmn/lith/issues/284) the default decides for you:** ratio 4 once the endpoint's measured first-byte latency is at or under **50 ms**, off above that, and off until a fill has measured it. Pass a positive value to force a ratio, or a **negative** value to force the gate off. Takes 54× over-fetch to 2.65× on the shape that motivated it; costs a fixed ~0.07–0.18 s on a fast consumer reading most of an object. See below. |
| `--coalesce-gap` | `0` (derived) | Largest gap between two projection/demand fill ranges still merged into one range GET (the byte-precise fill path; only active with `--footer-tier2`). `0` derives it from the device: **NIC baseline × measured first-byte latency ÷ usable concurrency**, clamped to `[256 KiB, 64 MiB]` — a round-trip's worth of bytes amortized across the concurrent requests in flight ([#124](https://github.com/scttfrdmn/lith/issues/124)/[#31](https://github.com/scttfrdmn/lith/issues/31)). Set a fixed size to override the derivation. |

**Reading `lith_readahead_evidence_ratio`.** It reports the evidence-gate ratio *in force*, which is not necessarily what you set. `0` means the gate is off — which is the default, and is also what a mount reports before any fill has measured the endpoint's first-byte latency. A non-zero value with no `--readahead-evidence-ratio` set means the latency-derived policy engaged. To tell the two `0` cases apart, read `lith_ttfb_measured` alongside it; `lith_ttfb_median_seconds` is the value the policy decided on.

### `--prefetch-coverage-min`, and why concurrent readers of one file never prefetch

Lith opens with `FOPEN_KEEP_CACHE`, so every descriptor on an inode shares the kernel page cache. When several readers stream the **same object**, one reader faults a range in and the others' reads of that range are served by the kernel and **never reach lith**. Each handle therefore sees an offset stream full of holes — all multiples of 128 KiB, the kernel readahead unit — and its trailing coverage is roughly `1/N`.

Against the default `0.5`, that means establishment dies between **2 and 4** concurrent readers, which is where `1/N` crosses the threshold. Measured on 16 readers of one 3.76 GB object: `lith_streaming_handles` stays at **0**, nothing is prefetched, every read becomes a demand GET, and the run takes **776 s against 3.19 s for one reader alone — 243×**. Byte amplification is **1.001**, the cleanest count in the whole experiment, so no byte or request counter can see it. With `O_DIRECT` the same 16 readers establish 16/16 and finish in 2.1 s.

**On a real workload this is present but is not where the bytes are.** A 48-rank GCHP run found all 12 met objects had interleaving sibling handles, and 252 of 262 substantive handles had one — yet handles below 0.5 coverage carried only **8.8–9.0%** of read bytes. So lowering this threshold is worth single-digit percentages there, not 243×.

**The trade.** Lowering it admits those handles — and also admits genuinely scattered walks, which is what the gate exists to stop: a FITS 2-D cutout over-fetched **4.76×** through exactly that path ([#222](https://github.com/scttfrdmn/lith/issues/222)). There is no setting that is right for both, which is why this is a flag and not a new default.

**Diagnosing it** needs `lith_prefetch_coverage_held_total` rising while `lith_prefetch_low_coverage_total` stays flat: contiguous progress refused a window, as opposed to a scattered landing forced Random. The held counter ticks at **block boundaries**, not per read, so its magnitude is roughly `bytes read / --block-size` when a handle is fully starved.

### `--readahead-evidence-ratio`, in detail

A handle establishes at its first **block crossing** — one block (8 MiB) of contiguous
evidence — and that buys the *full* NIC-derived window, ~223 blocks (~1.8 GB) on a 50 Gbps
node. With ratio `k` a handle prefetches ~`k`× what it has read, so a sequential copy still
earns the full window (after consuming `max-readahead × block-size / k`) while a reader that
tiles a slab and jumps does not.

**How much it helps depends on how many readers are streaming at once.** The window is
`clamp(prefetch-budget / streaming-handles, 2, --max-readahead)`. The measurement below was
taken when the divisor counted open **descriptors** instead, so a 48-rank job with ~600 open
handles sat on the **floor of 2** and the gate had almost nothing to give back — amplification
2.425× → 1.956× at `k=8`. Since [#301](https://github.com/scttfrdmn/lith/issues/301) only
handles actually being read sequentially count, so that job divides by ~48 rather than ~600;
the numbers here have not been re-taken against the new divisor. A **single** reader gets the full ~223-block
window, which is larger than most single objects, so an established sequential reader
prefetches the *whole file* however little of it it wants. On one process reading one variable
of a 1.22 GB NetCDF-4 file, over-fetch is **54×**, and over-fetch equals **1/coverage** to
within 2% across a 54× range of coverage. At `k=4` that becomes 1217.8 → **59.8 MB (−95.1%)**
with byte follow-through *rising* 0.005 → 0.135. The single-handle case — `python`, `xarray`,
`ncks`, `h5py`, one process against a bucket — is both the worse case and the more common one,
so an earlier claim here that the flag "does not improve prefetch precision" was a 48-rank
measurement stated as a general one.

**What it does not touch.** The cold-start granularity tax (the ~1.23 GB floor of
[#256](https://github.com/scttfrdmn/lith/issues/256)) is **bit-identical** with the flag on,
because the whole-chunk commitment is taken before any window decision exists. There are two
distinct waste channels and this addresses exactly one.

**The high-latency cost is `#229`'s establishment burst being capped, and it cannot be tuned
away.** On an object *larger* than the window (3.78 GB = 450 blocks) at 58.6 ms RTT the effect
is a clean, fully separated factor — median **8.51 s → 21.74 s (2.56×)** at `k=4`, zero
overlap across n=8 per arm (exact rank-sum p = 1.6×10⁻⁴) — while moving **identical bytes in
identical request counts** (3,776,834,855 B, 465 GETs) in every cell. In-region the same
effect is **+7.5%** (n=8, p ≈ 0.03).

The mechanism is a single dispatch event. `advance(cursor+1+window)` emits `[frontier, target)`
in one call, so the window value at establishment **is** the batch size — and
[#229](https://github.com/scttfrdmn/lith/issues/229) exists to make that batch the full window,
precisely to avoid "many small GETs, an underfed NIC on the cold read". With the gate off, one
read dispatches **223 blocks at once** and every later dispatch is 1 block (p99 = 1); that
single burst is the whole of the throughput advantage. With the gate on the burst is capped to
what consumption has earned, and nothing later recovers it:

| `k` | burst at establishment | throughput | concurrent streams' worth | median wall |
|---|---|---|---|---|
| off | **223 blocks** | 443.8 MB/s | **3.10** | 8.51 s |
| 40 | 41 | 283.8 MB/s | 1.98 | 13.31 s |
| 4 | 5 | 173.7 MB/s | 1.21 | 21.74 s |
| 1 | 2 | 160.4 MB/s | **1.12** | 23.55 s |

One stream is 8 MiB / 58.6 ms = 143 MB/s and a fully serial read is 465 × 58.6 ms = 27.2 s, so
`k=1` at 1.12 streams is **essentially serial**. That is also why raising `k` does not rescue
it: concurrency cannot fall below one stream, so a **40×** range in how long the cap binds buys
only a **3.1×** range in the penalty. The gate bounds committed readahead bytes, and on a long
pipe committed bytes *are* what buy concurrency — so the byte saving and the burst are the same
quantity, and no ratio keeps both.

Earlier reports of a *bimodal* 4 s / 20 s split came from a test object **smaller** than the
window, where readahead extent was never the binding constraint at all.

**A bytes-and-requests regression check is not sufficient for this flag.** Those are identical
on both sides of the effect, so such a check passes while wall clock varies by a factor of 2.8.

**So: use it in-region, or where you are paying for bytes and can afford the wall clock.** It
is not a fix for the granularity floor, and on a high-latency or non-AWS endpoint it costs
1.6–2.8× on reads of objects larger than the prefetch window — at every ratio tested, including
one permissive enough to clear the cap after 1.1% of the object.


**Since #284 this is on by default in-region, and that decision is measured.** Its cost falls entirely on one shape — a *fast* consumer reading *most* of an object, where the shallower ramp becomes the bottleneck — and that cost is sharply RTT-scaled:

| endpoint | slice reader | whole object, fast consumer | whole object, slow consumer |
|---|---|---|---|
| in-region, 2.2 ms | wins | **r = 1.05–1.21** | — |
| cross-region, 58.6 ms | **r = 0.84**, wins both axes | **r = 2.35** (zero overlap) | r = 1.00 |

Five cells, two boxes, warm and cold mounts. So the gate defaults **on** where it is cheap and **off** where it is not. The bound is a **first-byte latency, not a round trip** — v1.4.0 shipped 5 ms, derived from the 2.2 ms RTT, and in-region first-byte latency is 28.2 ms median (p90 42.7), so it never engaged ([#340](https://github.com/scttfrdmn/lith/issues/340)). It is now **50 ms**, anchored on both sides: above the whole measured in-region distribution, and below the 58.6 ms cross-region round trip that a first-byte latency cannot undercut. Read `lith_ttfb_median_seconds` to see which side a mount is on, `lith_ttfb_measured` to tell "not measured yet" from "measured and fast" (the median reads 0 for both), and the `lith_ttfb_seconds` histogram for the spread — `lith_ttfb_seconds_bucket{le="0.05"}` against `lith_ttfb_seconds_count` answers "will the default engage here" without estimating a quantile.

**The median is load-sensitive, and that is an open defect ([#349](https://github.com/scttfrdmn/lith/issues/349)).** Measured in-region, the median reads ~28 ms on an idle mount and **~100 ms during that same mount's own prefetch burst** — above the 50 ms bound *and* above the 58.6 ms cross-region round trip the bound's far side is anchored on. So a busy near endpoint and an idle far one are not separable on it, and because the gate's decision changes the burst depth that produces the reading, the policy is **bistable**: off keeps itself off. A single reader works today only by an accident of ordering — its serial demand GETs on blocks 0–1 flush the 8-sample window under the bound just before prefetch commits.

`lith_ttfb_floor_seconds` is the candidate replacement input: the 10th percentile over a 256-fill window, reported once **10** fills have completed (a single small read is only ~24 fills, so a higher minimum would make it unobservable on the very shape the gate exists for). It is a low quantile and never the smallest sample, so one anomalously fast fill cannot define an endpoint. **Its tolerance is explicit:** a p10 stays load-invariant while at least one fill in ten still gets an unqueued first byte; past that it rises, because with less evidence than that a stale low figure would be worse than the truth. How much saturation that survives is what #349's loaded cell measures — at 12 concurrent GCHP ranks, 49–54% of fills were still under 25 ms. Queueing can only *add* to a first-byte latency, so a floor is a lower bound on what the endpoint itself costs and load cannot raise it — while a far endpoint's floor cannot drop under its round trip, so the by-construction far-side anchor survives. **Nothing reads it yet.** It is exported so the deciding measurement — does a floor separate in-region-under-load from cross-region-idle? — can be taken before any policy moves onto it.

**The cost is fixed, not proportional.** Across a 14.5× object-size range the penalty stayed at +0.07–0.18 s; proportional would have made the 3.78 GB case +2.0 s and it measured +0.14 s. The *ratio* stays bounded (~+24% worst case in the exposed band) because the baseline carries its own fixed cost of roughly 0.5 s per object — which is **unexplained**, is 72% of a 260 MB read's wall, and is not the GET count: a cold sequential 64 MiB read issues 22 GETs, about 48 ms even fully serialized at 2.2 ms ([#233](https://github.com/scttfrdmn/lith/issues/233)).

**What it does not touch.** Objects at or below `--parts-max` take the whole-file parts fetch and never reach the windowed path. A handle the detector has not established never consults the cap, so a scattered or random reader is unaffected — and `evidence_held = 0` in a trace can mean the [#221](https://github.com/scttfrdmn/lith/issues/221) coverage gate got there first rather than that this one did not bind. The two are in series, coverage first.

**Reading it:** `lith_readahead_evidence_ratio` reports the ratio in force, which is not necessarily what you set — `0` includes a mount that has not yet measured its endpoint. `lith_prefetch_evidence_clamped_total` and `lith_prefetch_evidence_withheld_blocks_total` are per-read as of [#322](https://github.com/scttfrdmn/lith/issues/322); before that they were folded in at close and read zero for the whole life of a job that holds its handles open.
## Archives and published datasets

Mount a [CargoShip](cargoship.md) archive, or a [published dataset](published-datasets.md) by name.

| flag | default | why |
|---|---|---|
| `--cargoship` | — | Mount/serve a CargoShip 2.1 archive: build the index in-process from this manifest `s3://` URL and present the archive's original file tree (reads fetch only the covering zstd frame(s)). Fails closed — a manifest that cannot be resolved is an error, never a fallback to listing. |
| `s3://bucket/dataset@current` / `@<version>` | — | A published-dataset pointer mount (no flag): resolve the atomic `CURRENT` pointer (or a pinned version) and mount its prebuilt index. See [Published datasets](published-datasets.md). |
| `lith refresh <mountpoint>` | — | Re-read `CURRENT` and hot-swap a FUSE mount to a new version (explicit; no polling). The gateway does not hot-swap — restart it to adopt a new version. |

## Gateway (`lith serve nfs`)

| flag | default | why |
|---|---|---|
| `--listen` | `:2049` | Listen address for the NFSv3 + embedded MOUNT service. The gateway never registers with portmap/rpcbind; clients mount with an explicit `port=`/`mountport=`. |
| `--client-idle` | `5m` | Release a mounted client's share of the read-ahead budget after it has been idle (since its last MOUNT) this long. |

## Operational and diagnostics

| flag / command | default | why |
|---|---|---|
| `--log-level` | `info` | Log verbosity: `debug`/`info`/`warn`/`error` (on `mount` and `serve nfs`). `debug` surfaces the format-plan diagnostics. |
| `--daemon` | off | `lith mount` forks into the background once the mount is ready. |
| `lith doctor [s3://…] --mountpoint` | — | Diagnose whether lith will work here — credentials, bucket access/region, LIST, FUSE, and the given `--mountpoint` (exists, a dir, owned by you, empty). Exits non-zero on any failure. See [the doctor](#the-doctor). |
| `lith mounts --prune` | off | List active mounts; `--prune` drops stale records whose process is gone. |
| `lith umount --all` / `--force` / `--timeout` | — | Unmount one, or `--all`; `--force` lazy-unmounts a busy mount; `--timeout` bounds the wait. |

### The doctor

`lith doctor` runs the checks above (add a bucket to include bucket/region/LIST/pointer checks, `--index-file` to validate an index) and prints each as `PASS`/`FAIL`/`N/A` with a one-line fix. Run it first on a new box — it catches the failures that otherwise surface as a cryptic mount error (wrong region, a root-owned mountpoint, a LIST-denied bucket, missing FUSE).

## Benchmark (`lith bench`)

| flag | default | why |
|---|---|---|
| `--against` | — | Comparison reader to run alongside (e.g. `mountpoint-s3`) for an apples-to-apples table. |
| `--pattern` | — | Access pattern to exercise (`seq`, random, projection, …). |
| `--ops` / `--runs` | — | Operations per run / number of runs. |
| `--readers` | `1` | Concurrent readers (multi-reader mode; requires `--objects`). |
| `--objects` | — | Comma-separated keys for multi-reader mode. |
| `--cache-dir` | `$TMPDIR` | Directory for the bench block cache. |
