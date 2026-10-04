# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **M17-D: the frame cache under eviction, and two test defects of my own**
  ([#219](https://github.com/scttfrdmn/lith/issues/219)). The frame-cache seam is **clean**;
  the two defects were in the tests meant to guard #332.

  `TestFrameCacheConcurrentRace` verified content under concurrency but with a budget large
  enough to hold every frame, so the LRU never ran — and both of the frame cache's documented
  edges are eviction-shaped (least-recently-used frames dropped while other goroutines hold the
  decoded slice; a frame larger than the whole budget served to waiters but never retained).
  Both now run under concurrency with content verified: 32 workers × 40 reads over 8 frames
  with a 2-frame budget, and 24 concurrent readers of an un-retainable frame. Clean under
  `-race`, repeated.

  **And two assertions of mine were wrong, in the same direction.**
  `TestColdSequentialGetShape`'s total-GET bound of 20 was tightened onto a **load-sensitive**
  number — background prefetch racing the read loop decides whether a demand read also issues a
  GET — and it flaked in the full-tree run. That is the measurement-discipline rule applied to
  a *bound* rather than a figure: it needed its distribution too. The total is now logged with a
  bound that only catches degeneration to per-chunk fetching.
  `TestEstablishmentDispatchesTheCurrentBlock`, which I had cited as the tight deterministic
  backstop, asserted `dispatched[0] > 2` — and the pre-#332 value is exactly 2, so **it passed
  with the fix reverted.** Now asserts equality against the block establishment happened on,
  verified by reverting #332 and watching it fail.

- **M17-D, gateway seam: the NFS readahead divisor counts MOUNTS, not readers**
  ([#219](https://github.com/scttfrdmn/lith/issues/219),
  [#337](https://github.com/scttfrdmn/lith/issues/337)). Characterized, not fixed.

  `windowBlocks()` divides the prefetch budget by `activeClients()`, which counts clients that
  sent a MOUNT within `ClientIdle` (5 minutes by default). A client that mounts and reads
  nothing holds a share for those five minutes, so an actively-reading client's window
  collapses **48×, from 96 blocks to the 2-block floor, from mounts alone**.

  That is #301 in the gateway: there the divisor counted every open file descriptor including
  never-read ones, and a job merely holding files open drove every reader to the same floor at
  6–10× the wall clock. The fix was not to remove the division but to change its input.

  The gateway's analogue of that input already exists — `roFS.states` carries a per-path
  `lastTouch`, updated in `stateFor`, which NFS's statelessness calls on **every read**. The
  code's stated reason for measuring from MOUNT ("per-op client activity is not visible through
  go-nfs's stateless read path") is true of go-nfs and not of lith's own state map.

  Left as a characterization because changing a prefetch divisor is the change class that
  produced an 11× regression in this project, and the gateway's throughput cannot be measured
  here. The test becomes the assertion when the input is fixed.


### Fixed

- **`Open` built a file handle from a MIX of two index versions during a refresh**
  ([#219](https://github.com/scttfrdmn/lith/issues/219)). `rawFS.Open` loaded the index three
  separate times — `Stat` for the size, `ETagHashOf` for the cache key, `BackingOf` for the
  cargo parts. Each load is individually safe (the index is an atomic pointer), but a
  concurrent `SwapIndex` landing between two of them gave the handle **a size from one version
  and a cache key from another**. The handle then read bytes bounded by one version's length
  under the other version's ETag.

  **Measured at 33% of opens** — 10,438 of 32,000 — under continuous swap. `-race` reports
  nothing, because every load is correct in isolation; this is the class #219 was opened for,
  where a race serves wrong bytes rather than crashing.

  `liveIndex` is a struct behind one atomic pointer precisely so a single load yields a
  coherent view. `Open` now takes it once, and so do `readdir` (which had four loads, two
  inside the entry loop, so a swap mid-listing could return entries from one version with
  attributes from another — and `ReadDirPlus` registers those with the kernel, which caches
  them) and `StatFs` (two loads, so `df` could disagree with itself). Every FUSE op now takes
  exactly one.

### Added

- **M17-D: a seam stress harness that verifies BYTES, not counts**
  ([#219](https://github.com/scttfrdmn/lith/issues/219)). Every concurrency test in the
  blockstore checked counts, states, or the absence of a panic; none checked that a concurrent
  reader got the right bytes — which is the only way the failures #219 targets are visible.

  The object's byte at offset *i* is a fixed function of *i*, so a read served from the wrong
  offset, a zero-filled extent, or a boundary assembled from two fills is detectable, and the
  failure message reports which offset the data actually came from and whether the shift is a
  whole chunk, an extent, or neither.

  The first seam — extent fills and whole-chunk fills racing on the same chunk, with a tier a
  third of the object so eviction runs concurrently with the fills — is **clean**: 48 workers ×
  400 iterations under `-race`, 1538 GETs, every byte verified. A clean run is a passing result.
  The second seam found the `Open` tear above.


## [1.4.0] - 2026-10-04

### Added

- **`--timeline-csv` records handle opens, so the per-open cost is measurable**
  ([#284](https://github.com/scttfrdmn/lith/issues/284)). An external deployment fitted a
  **0.47 s per-object-open** intercept on a 1.5 GB/s box — 72% of a 260 MB read's wall — and no
  instrument could show where it went. `--pf-trace` has **no timestamp column at all** (its
  `seq` is a monotonic decision counter), and `--timeline-csv` had timestamps but nothing
  marked the open, so the gap could only be inferred from process start.

  An open is now a row with `kind=open`, carrying the object size — so the CSV schema is
  unchanged and an existing consumer sees one more kind value rather than a different file.
  The interval from that row to the first chunk row for the same key is the per-open cost, read
  directly. Plumbed as a *separate* optional recorder interface, asserted independently, so no
  existing `Recorder` implementer has to change.

- **The mount logs its FUSE transport, for symmetry with its S3 transport**
  ([#232](https://github.com/scttfrdmn/lith/issues/232)). `max_read_bytes`,
  `reads_per_chunk`, `max_background`, `congestion_threshold`, the kernel's
  `max_readahead` and the negotiated protocol version. The S3 side has always been
  observable at mount; this side never was.

  The number worth noticing is **`reads_per_chunk = 8`**: go-fuse sets `max_read` equal to
  `MaxWrite`, which defaults to 128 KiB, so a 1 MiB chunk reaches the application in eight
  FUSE round trips — seven of them cache hits returning a sub-slice. That is deliberate and
  measured (raising it to 1 MiB made each reply exceed go-fuse's splice pipe and forced a
  copy, a net loss for the CPU-bound multi-reader path), and `internal/fuse/mount.go` now
  records all three transport limits together rather than only that one, so the next person
  to wonder about `max_background` does not have to re-derive the other two.

  `max_background` is a go-fuse default of 12 that lith has never set and nobody has measured.
  It bounds **kernel-initiated** readahead to ~1.1 MB in flight mount-wide; it does not bound
  application reads, and lith's own prefetch is unaffected because it runs as goroutines
  against S3 rather than as FUSE requests. Logged rather than changed.

- **`lith_readahead_evidence_ratio`, and the latency-derived evidence policy's plumbing**
  ([#284](https://github.com/scttfrdmn/lith/issues/284)). **No behaviour change**: the policy
  returns 0 at every latency and a test asserts it.

  `--readahead-evidence-ratio` bounds a committed readahead window to a multiple of the bytes a
  handle has actually consumed, and it is the one thing measured to fix #284 — where a single
  process reading one variable of a multi-variable NetCDF-4 file fetched the **whole object**,
  54.03× over-fetch, equal to `1/coverage` to within 0.4% on two different objects. It is off by
  default because its cost is sharply RTT-scaled: byte-identical and wall-indistinguishable
  in-region, and a clean **2.56×** at 58.6 ms.

  The ratio is now consulted **per read** from the endpoint's measured latency rather than
  latched at open from the flag, so the policy has somewhere to live; the gauge reports which
  regime a mount landed in. The constants wait on a measured ladder between those two anchor
  points — this campaign has refuted five mechanisms and shipped one 11× regression whose
  justifying argument agreed with the thing it replaced to within 2.4%.

### Changed

- **#222's over-fetch is attributed, and the obvious fix is refuted by measurement**
  ([#222](https://github.com/scttfrdmn/lith/issues/222)). No behaviour change — a
  characterization test.

  A strided slice reader pays **a whole 1 MiB chunk per 4 KiB read — 256×**. Two mechanisms
  exist to stop exactly that and neither reaches `Strided`: the byte-exact extent lane gates on
  `state() == Random`, and the evidence gate's `windowCap()` is consulted only on the
  Sequential ramp. So extending the demand lane to `Strided` looks like a one-line fix.

  **It is not.** Measured: bytes went **up** by ~14.9 KB per read, because the cost is the
  strided branch's *prediction*, not the demand read — the branch dispatches one predicted block
  per read and fetches it whole, and with the demand lane extended the extents were then fetched
  on top of a prediction that already covered them. The prediction is also *correct* about which
  block: at stride `d` it predicts `blk+d`, exactly the next read's block. The branch is right
  about where the reader is going and wrong about how much of it the reader wants.

  Pinned by a test, with the companion bound that any real fix must respect (a strided *bulk*
  reader is served correctly today and must stay that way). The fix needs the prediction bounded
  byte-exactly, and there is no byte-range prefetch entry point today — `BlockStore.Prefetch`
  takes a block index — which is why it is not a one-liner.

- **The readahead evidence gate is on by default in-region**
  ([#284](https://github.com/scttfrdmn/lith/issues/284)). **A behaviour change**, and the first
  one to the prefetch path since the #301 revert.

  `--readahead-evidence-ratio` bounds a committed readahead window to a multiple of the bytes a
  handle has actually read. A single process reading one variable of a multi-variable NetCDF-4
  file was measured fetching the **whole object** — 1217.8 MB for the 22.5 MB it wanted,
  **54.03×**, with the over-fetch equal to `1/coverage` to within 0.4% on two different
  objects. At ratio 4 the same read fetches 59.8 MB: **2.65×**, and 37 GETs instead of 175.

  `0`, the default, now **decides from measured first-byte latency**: ratio 4 at or under 5 ms,
  off above it, and off until a fill has measured the endpoint — never derived from the 40 ms
  seed. A positive value forces a ratio; a **negative** value forces the gate off, since 0 no
  longer means that.

  **Why latency-gated rather than simply on.** The cost falls on one shape — a fast consumer
  reading most of an object, where the shallower ramp is the bottleneck — and is sharply
  RTT-scaled:

  | endpoint | slice reader | whole object, fast consumer |
  |---|---|---|
  | in-region, 2.2 ms | wins | r = 1.05–1.21 |
  | cross-region, 58.6 ms | **r = 0.84**, wins both axes | **r = 2.35**, zero overlap |

  Five cells, two boxes, warm and cold mounts, every arm interleaved. The 5 ms bound is
  deliberately conservative — a little over twice the measured-good point — because **nothing
  is measured between 2.2 and 58.6 ms** and picking a threshold in that gap is what this
  campaign has repeatedly been punished for. A mount at 20 ms keeps the behaviour it had.

  The cost is also **fixed rather than proportional**: +0.07–0.18 s across a 14.5× object-size
  range, where proportional would have made the 3.78 GB case +2.0 s and it measured +0.14 s.
  Two of the author's predictions were refuted on the way to this (a per-mount intercept, and a
  2.2–3.4× ratio in the 64 MiB–1 GB band); the shipped bound is the one that survived.

- **#233's cold-start cost is pinned by a test, and it rules out the leading explanation for
  #284's per-open intercept** ([#233](https://github.com/scttfrdmn/lith/issues/233),
  [#284](https://github.com/scttfrdmn/lith/issues/284)). No behaviour change.

  A cold sequential reader's 128 KiB reads stay inside one 8 MiB block for 64 of them, so the
  detector never sees a block advance and establishment cannot fire until the reader crosses
  into block 1 — leaving block 0 served as **eight 1 MiB chunk GETs** instead of one coalesced
  fetch. Measured offline: establishment at read 64 exactly, 8 GETs inside block 0, 22 GETs
  total for a byte-exact 64 MiB read. #233 recorded this from hardware and listed three
  refuted fixes; it now has a regression test rather than only a description.

  **The negative result is the more useful half.** An external deployment fitted a per-open
  intercept of **0.47 s** on a 1.5 GB/s box — 72% of a 260 MB read's wall — and window
  establishment on each new handle was the leading hypothesis on both sides. 22 GETs is ~48 ms
  even fully serialized at that deployment's measured 2.2 ms first-byte latency, two orders
  short of 470 ms. So the intercept is not the GET count and not establishment.

- **`mmap` random access is characterized, and the amplification is mostly not lith's**
  ([#232](https://github.com/scttfrdmn/lith/issues/232)). No behaviour change — a
  characterization test plus the documented finding.

  A hypothesis of mine is refuted by it: I expected a sustained random walk to oscillate into
  the detector's `Cold` posture, where the #210 byte-exact gate (`state() == Random`, exactly)
  fails and a 4 KiB read buys a whole 1 MiB chunk. It does not — 1121 of 1126 reads saw
  `Random`, and 968 were served byte-exact against 5 whole-chunk. The extent lane holds.

  What the numbers do say: lith is **faithful to the read it is handed** (0.79–0.81× the
  requested bytes), and its floor is one 64 KiB extent — so a 4 KiB read costs 64 KiB (15.3×)
  while a 4 KiB *touch* costs whatever the kernel asked for. On the reported run that was
  ~220 KiB per touch, of which only ~3.4× was extent rounding and the rest was kernel
  readahead. Both ratios are stable across a 4× fixture change.

  So the 326 s is round trips, not bytes: a single-threaded fault stream is serial by
  construction and there is nothing for lith to overlap. Reducing lith's granularity would
  address a small fraction of the bytes and none of the wall clock.

### Fixed

- **On establishment, prefetch the block the reader is IN rather than starting past it**
  ([#233](https://github.com/scttfrdmn/lith/issues/233),
  [#284](https://github.com/scttfrdmn/lith/issues/284)). This is roughly half of the ~0.47 s
  per-object-open cost that #284 could not account for.

  Establishment fires on the first **block advance**, so at that moment the reader has just
  crossed into the current block with most of it still ahead — 63 more 128 KiB reads of an
  8 MiB block. The frontier was clamped to `cursor+1`, so every one of those was served as a
  separate demand chunk GET and **block 1 repeated block 0's cost exactly**.

  Measured on a real mount with `--timeline-csv`: `lag_ms` was **−1 for chunks 0–15 in every
  one of 12 opens** — none of the first sixteen chunks was ever prefetched — with the reader
  stalled 206–431 ms in block 0 and a further 245–389 ms in block 1, out of a 569–957 ms read.
  The first prefetched block was block **2**.

  Offline, a cold sequential 64 MiB read drops from **22 GETs to 17** (both stable across six
  runs; the post-fix figure occasionally reads 18). Block 0's 8 are irreducible — establishment
  cannot fire before a block advance, and #231 refuted three ways to establish sooner — so the
  five recovered are block 1's. At the ~28 ms in-region first-byte latency for a 1 MiB GET that
  is **~140 ms per open**.

  Dispatching from the cursor costs nothing where the block is already covered: the chunk
  singleflight joins a demand fill in flight rather than duplicating it. Applied at
  establishment only — a handle mid-stream has its frontier well ahead, so the clamp never
  fires for it.

- **The timeline's `prefetched` column was hardcoded `false`**
  ([#284](https://github.com/scttfrdmn/lith/issues/284)). `emitChunk` received a literal
  `false` on every hit and uncovered row, so `--timeline-csv` reported `prefetched=false` even
  for hits whose `lag_ms` proved they had been prefetched. Now read from `bs.prefetched`
  *before* `fetchExtents` credits the prefetch and drops the marker — read afterwards it is
  always false. Found in the field on the first run that used the column.

- **A stride must now repeat twice before the detector believes it**
  ([#222](https://github.com/scttfrdmn/lith/issues/222)). The Strided branch declared a
  pattern on **one** repeated block delta, with no byte-gap bound (unlike the sequential
  branch beside it) and no coverage check — and deliberately before the seek rule, because a
  large gap is what a stride is. So its entire evidence was one coincidence.

  That is structural, not theoretical: over a walk spanning `B` blocks, consecutive deltas
  collide with probability ~`1/B`, so `N` reads yield ~`N/B` false strides. A **genuinely
  random** `mmap` walk was measured flipping Random → Strided **7 times in 2471 reads**, on
  gaps of +55 MB, +166 MB and +131 MB, dispatching prefetch for +27 MB over distinct
  ([#232](https://github.com/scttfrdmn/lith/issues/232)).

  Requiring two repeats takes the rate from ~`1/B` to ~`1/B²`. Measured: **0 flips across 8
  random walks** of 2471 reads where one repeat predicted ~22 per seed, and the random-walk
  harness's own byte total fell 4–6%, against the 7% over-fetch measured on hardware. A real
  strided reader — a FITS cutout walking row segments, a hyperslab — meets two repeats
  trivially and establishes exactly one read later.

  The run gates **entry only**. Re-counting on an established handle made it predict on
  alternate reads (11 dispatches over 24 strided reads instead of 21); that was a bug in the
  first version of this fix, caught because the test asserts dispatch volume and not just the
  final state.

- **`--mem-cache` below 64 MiB no longer silently disables the memory tier**
  ([#307](https://github.com/scttfrdmn/lith/issues/307)). The tier was a fixed 64 shards, and
  `mem2Q` refuses any store larger than a shard's capacity — so any `--mem-cache` giving
  shards under one 1 MiB chunk accepted **nothing**, with no error, no warning and no metric:

  | `--mem-cache` | per shard | |
  |---|---|---|
  | 16 MiB | 256 KiB | **dead** |
  | 32 MiB | 512 KiB | **dead** |
  | 64 MiB | 1 MiB | the exact boundary |

  The default is 25% of system RAM, so the threshold was a machine with **256 MiB** — a
  container, a CI runner, a constrained sidecar. Below it every read missed, every re-read
  re-fetched, and `lith_mem_hit_total` sat at zero, which reads as a cold workload rather than
  a broken tier.

  `newMemCache` now reduces the shard count until each shard holds at least one chunk, so a
  16 MiB tier gets 16 shards rather than 64 dead ones. That trades lock contention for a
  working cache, which is the right trade: a tier too small to give 64 shards a chunk each is
  also too small for 64 readers to contend over. A capacity below **one chunk** cannot be
  fixed by scaling and the mount now warns about it explicitly, distinguishing it from
  `--mem-cache 0`, which is caching deliberately disabled.

  The mount also logs the realized geometry (`shards`, `bytes_per_shard`, `chunks_per_shard`),
  because the defect was undetectable from the outside. It was found by shrinking a test
  fixture, where three assertions broke for a reason that made no sense until the tier turned
  out to be inert.

- **`lith_prefetch_committed_bytes` now says what it counts**
  ([#320](https://github.com/scttfrdmn/lith/issues/320)). Resident-unread **plus
  queued-for-a-slot plus on-the-wire**. The charge happens in `Prefetch`, *before* `fillRun`
  waits on the prefetch and S3 semaphores, and `Prefetch` runs one goroutine per block — so
  the figure includes chunks that are merely queued, and **`--inflight-bytes` does not bound
  it**, because that bounds the wire and not the queue.

  Comparing the two is what made an external measurement read a 9–27 GB "leak" that did not
  exist. At rest the figure equals `lith_prefetch_unread_resident_bytes` exactly, measured in
  four isolated cells; use that one to judge memory-tier pressure, since it is the only part
  that can evict anything. `Release` does ratchet the committed figure, but opens no gap: a
  closed handle's unconsumed chunks are still unread-resident and both gauges hold them.

- **Four paths consumed or superseded a prefetched chunk without crediting it**
  ([#320](https://github.com/scttfrdmn/lith/issues/320)). All four reproduce; all four are
  fixed. Two were found by an external deployment reading this code *after* it retracted a
  9–27 GB "leak" that turned out to be dispatch-side, and which its own cells had not
  provoked — straddle was 0 and the demand/prefetch race never fired.

  | path | effect |
  |---|---|
  | `ensureChunks` tier hit | a **straddling `GetRange`** served from a prefetched chunk credited nothing |
  | `ensureChunks` join | a demand read that joined a prefetch's fill credited nothing |
  | `complete`, demand fill | a demand fill landing a chunk a concurrent `Prefetch` had committed left it charged **forever** |
  | `fetchExtents` | a **prefetch** finding its own chunk resident credited itself a demand hit |

  **This is not only a reporting fix.** A consumed chunk left flagged unread is protected by
  the #55 eviction preference as though nothing had read it, so the tier keeps a dead chunk in
  preference to a live one. Conversely the fourth path *cleared* the flag on a chunk nothing
  had read, handing it to eviction early and releasing a budget reservation still owed. And
  `lith_prefetch_unread_resident_bytes` is the threshold an external measurement validated 8/8
  against the collapse condition ([#313](https://github.com/scttfrdmn/lith/issues/313)) — on a
  straddle-heavy workload it would have read high for the wrong reason.

  The third path is the only shape that can make `committed` exceed `unread-resident` **at
  rest**: nothing would ever release it — not consume (the demand owner credits nothing, having
  filled the chunk itself), not unread-evict (it is not flagged), not failed fill (it
  succeeded). Measured at rest, the two gauges are now equal to the byte.

  `creditPrefetch(k, ci, isPrefetch)` replaces the bare `notePrefetchHit` at every site, so
  the demand-only guard is in one place rather than remembered at four.

- **Every per-handle prefetch counter is now recorded per read, not at close**
  ([#284](https://github.com/scttfrdmn/lith/issues/284),
  [#316](https://github.com/scttfrdmn/lith/issues/316)). #319 fixed this for the two coverage
  counters; the remaining five — window halvings, random resets, evidence-gate holds, blocks
  withheld, and de-establishments — were still folded into their metrics once per handle at
  `Release`. On a job that holds its handles open for its whole run, which is the normal shape,
  every one of them read **zero** until the process exited. An external 48-rank deployment
  measured exactly that.

  The labelled children are resolved once at `Open` and kept on the handle, so a per-read
  record is a few atomic adds rather than a `WithLabelValues` lookup under the registry mutex —
  and the labelled series now exists from the first **open** rather than the first close, which
  is the present-and-zero-versus-absent trap of #253 closed one step earlier.

- **`BlockStore.MeasuredTTFB` distinguishes a measurement from the seed**
  ([#292](https://github.com/scttfrdmn/lith/issues/292)). `currentTTFB()` falls back to a
  hard-coded 40 ms until a fill has recorded a sample, so anything deriving a value before the
  first fill received a plausible constant identical on every endpoint. #291 shipped a readahead
  floor derived that way and measured it at exactly 30 blocks on both a 2.2 ms and a 58.6 ms
  endpoint, where the real 58.6 ms round trip wants 44. The new accessor returns
  `(duration, measured bool)`, so a caller has to decide what to do about "not measured yet"
  instead of being handed a number that looks right.

## [1.3.0] - 2026-10-02

### Changed

- **An establishing handle sizes its window from the count it is about to join**
  ([#313](https://github.com/scttfrdmn/lith/issues/313)). `perHandleWindow` runs *before* the
  `Observe` that establishes its caller, so a handle that is not yet counted divides by
  `streams + 1`. Without it a sole establishing reader divides by 1 and takes the whole
  budget. An already-established caller still divides by `streams` exactly — adding one there
  would tighten every steady-state share (16 readers would get `492/17 = 28` blocks where the
  correct share is `492/16 = 30`, which an external deployment measured exactly).

  **This does not make the concurrent start safe, and a claim in this code that said otherwise
  was wrong.** It read: *"the geometric ramp bounds the burst, because a handle's first
  dispatch is 2 blocks and not maxReadahead"*. It bounds the first dispatch, not the ramp's
  integral — sixteen ramps running together were measured at 12.944 GB committed within 6 s,
  against a 4.128 GB budget and an 8.256 GB tier, producing a fairness collapse in which 3 of
  16 readers crawled to 106 s while 13 finished on schedule. Tracked as #313 with four
  candidate repairs and none shipped on argument.

- **The prefetch divisor counts sequential streams, not open file descriptors**
  ([#301](https://github.com/scttfrdmn/lith/issues/301)). This is the fix for #301; the
  division itself stays.

  `perHandleWindow` rations readahead as `clamp(prefetchBudget/blockSize / N, 2,
  --max-readahead)`. `N` was `len(f.handles)` — every open **descriptor** on the mount, across
  processes, per descriptor rather than per object, **including descriptors never read**. The
  charge was therefore linear in a number the reader does not control: at 256 descriptors a
  mount was charged 4295 MB, held 17.8 MB, and was throttled 6–10× for the difference. The
  workload that found this ran both production mounts at the 2-block floor — 48 ranks × ~6
  files is ~288 descriptors against the ~165 that gets you there — reading 186 MB/s where 1293
  was available, with no flag set wrong. It is also why `--max-readahead` looked inert through
  a dozen gates: the knob was never reachable from the workload.

  `N` is now the number of handles the detector is actually prefetching for — `prefetch.Sequential`,
  which is exactly the state in which a window exists. An idle descriptor contributes nothing.
  On the reporting workload's shape that is ~48 rather than ~288, so each reader's share rises
  about 6×, and the discipline is unchanged: all 48 active readers still get a share.

  **The division is kept, and both alternatives to it were measured first.** Removing the
  rationing outright costs 13.8–22.4× wall and 4.2–5.2× the bytes with 81–92% of prefetch
  evicted unread. Replacing it with byte-exact admission on the same total — which agreed with
  the divisor's realized commitment to within 2.4% — shipped briefly and regressed concurrent
  readers 5.66×, because a share is an allocation discipline and a total is not one.

  Maintained as deltas from each read's detector transition rather than by scanning handles:
  `perHandleWindow` runs on every read and the reporting workload has 6229 handles, so an
  O(handles) scan taking each handle's lock per read would cost more than the misallocation it
  fixes. `Release` gives the share back, idempotently.

  **Known limitation.** A handle that establishes, reads a little, then idles for the rest of
  the run keeps its share until it closes or the detector collapses it to Random. That still
  over-charges, by far less than counting never-read descriptors did. Shedding it needs a
  read-idle condition, which is a question on #301 rather than a mechanism guessed at here.

- **The mount reports all three bounds on outstanding prefetch, with the binding one named**
  ([#298](https://github.com/scttfrdmn/lith/issues/298)). One handle's window commitment
  (`--max-readahead × --block-size`), the mount-wide `--prefetch-budget`, and
  `--inflight-bytes` are derived from three unrelated quantities — an empirical multiple of the
  bandwidth-delay product, a fraction of RAM, and NIC × latency — and in the shipping default
  they **disagree by 1.5×**, with the smallest winning silently. They were logged separately,
  which left a tuner turning a knob that was not in play: raising `--max-readahead` from 223 to
  492 once measured **+3%**, because both configurations were already against a ceiling neither
  of them set. Not forced into agreement — each is legitimate on its own terms — just named.

- **Two corrections to `prefetch.Limits`' own documentation**
  ([#301](https://github.com/scttfrdmn/lith/issues/301)). It described the budget as "for
  prefetch not yet demanded", but its four callers Reserve before a fetch and Release when it
  *completes*, so `reserved` measures bytes being fetched rather than bytes held unread. And it
  said per-handle readahead "sizes its window from `Budget()`", which is no longer true — that
  sizing was the open-descriptor divisor. There are now two disciplines on purpose:
  fetch-scoped reservations for the bounded up-front fetches, and consumption-scoped admission
  for the unbounded windowed path.

- **The prefetch divisor stays, and the attempt to remove it is reverted**
  ([#301](https://github.com/scttfrdmn/lith/issues/301)). `perHandleWindow()` rations every
  handle's readahead as `prefetchBudget/blockSize / openHandles`, and `openHandles` counts
  every open file **descriptor** on the mount — across processes, per descriptor rather than
  per object, **including descriptors never read**. A job holding files open drives its own
  and every other reader's readahead to the floor of 2 blocks whatever the flag says. That is
  real, it is linear in the descriptor count, and it is why `--max-readahead` looked inert
  through a dozen gates.

  The fix shipped briefly and was wrong. Admitting prefetch against measured committed bytes
  (`ceeb2b7`) bounds the same **total** byte-exactly — within 2.4% of the divisor's realized
  commitment on a 16-reader tier — and in isolation it separated cleanly: 2.8× at 64 held
  descriptors, 4.5× at 256, identical bytes and GETs. The reporting workload then ran it on
  concurrent readers and measured **5.66× slower on exactly that arm, up to 11× elsewhere**,
  with both pre-registered falsifiers clean.

  What the divisor provides is not a total but an **allocation discipline**. 16 × 30 blocks
  covers sixteen readers shallowly; 2 × 223 + 14 × 0 commits the same total and covers two.
  Equal totals, opposite outcomes — and first-come-first-served admission produces the second.
  The 2.4% agreement that justified the change compared totals, so it could not have detected
  this. Reverted on measurement, and the over-charge stays open: the repair is to the
  divisor's **input** (established sequential streams, not open descriptors), which keeps the
  discipline and drops the error.

  `lith_prefetch_committed_bytes` and `lith_prefetch_budget_bytes` are kept — they are what
  made both the defect and the regression visible — and now report a quantity that is measured
  but not enforced. `lith_prefetch_refused_total` is removed with the admission it counted.

### Added

- **`--prefetch-coverage-min`: the #221 coverage threshold, as a knob**
  ([#316](https://github.com/scttfrdmn/lith/issues/316)). Experimental, default unchanged.

  The shipping `0.5` comes from one characterization (streams ≥ 0.89, scattered walks ≤ 0.07)
  and does not account for concurrent readers of **one** object: `FOPEN_KEEP_CACHE` shares the
  inode page cache, so each handle's siblings' reads never reach lith and its own coverage is
  ~`1/N`. Establishment therefore dies between 2 and 4 concurrent readers — 243× slower at 16,
  with byte amplification of 1.001.

  Lowering it admits those handles and also admits genuinely scattered walks, which over-fetch
  (4.76× on a FITS cutout, #222). No setting is right for both, which is why it is a flag and
  not a new default. Added so the threshold can be measured at zero cost rather than requiring
  a patched build.

- **`lith_prefetch_low_coverage_total`: the counter that makes a non-establishing handle
  explicable** ([#316](https://github.com/scttfrdmn/lith/issues/316)). Reads the #221 coverage
  gate forced Random. `Prefetcher.LowCoverage()` has existed since #221 and was never wired to
  a metric.

  It is the signal that separates two states nothing else distinguishes: a handle that is a
  scattered walk, as designed, from a handle that is a **dense stream whose sibling reads were
  absorbed by the shared kernel page cache**. The second was measured at **243× slower** with
  byte amplification of **1.001** — the cleanest byte count of any cell in that gate and the
  slowest per distinct byte. Bytes, requests and `prefetch_issued_total` all look correct or
  better; this counter and `evicted_unread` are the only things that move.

- **`lith_prefetch_unread_resident_bytes`: the quantity eviction-before-read is actually
  about** ([#313](https://github.com/scttfrdmn/lith/issues/313)). Bytes held in the memory
  tier that nothing has read, read from the tier itself.

  `lith_prefetch_committed_bytes` counts from *dispatch*, so it sums resident-unread **and**
  still-in-flight — and only the resident half can evict anything. An external measurement
  found committed/tier separating clean from collapsed runs at **1.20 vs 1.21** across two
  boxes 12× apart in RAM, a suspiciously tight edge for a ratio whose numerator includes
  bytes that cannot cause an eviction, and could not explain why committed plateaued near
  1.55× tier. Both resolve if `committed ≈ resident-unread + in-flight`: resident-unread
  cannot exceed the tier by construction, so the plateau is the tier plus what is in flight,
  and the real condition is **resident-unread approaching tier capacity** — at which point
  every arriving chunk must evict an unread one. That is a mechanism rather than a fitted
  threshold, and this gauge is what lets it be tested.

  Also immune to a known inaccuracy in the committed figure: prefetch commitment is never
  released when a handle closes, so committed ratchets on a mount whose working set fits the
  tier. This is read from the tier and cannot.

  Maintained incrementally across six call sites (mark, clear, three merge paths, both
  eviction paths), because a scan would be `O(resident)` under a shard lock. Guarded by an
  invariant test that recomputes from the authoritative state after 4000 randomized
  operations — that class of counter has drifted in this package before.

- **`lith_streaming_handles`: the divisor, as opposed to the descriptor count**
  ([#301](https://github.com/scttfrdmn/lith/issues/301)). Handles being prefetched for.
  `lith_open_handles` is kept alongside it and no longer sizes anything; the **gap** between
  them is the diagnostic — on the workload that found #301, ~288 descriptors against ~48
  streams, which is precisely what the old divisor over-charged for. A deployment can now see
  that directly instead of inferring it from wall clock.

- **The mount says when readahead is at the floor, and how many descriptors it takes to get
  there** ([#301](https://github.com/scttfrdmn/lith/issues/301)). The `prefetch bounds` line
  gains `full_window_descriptors` (how many concurrent open descriptors can each hold the
  whole effective window) and `floor_at_descriptors` (the count at which every reader is down
  to the 2-block floor). At the shipping default on a 33 GB box those are **2** and **165**.

  More useful than either: a one-per-mount `WARN` emitted at the moment a share actually hits
  the floor. The prediction needs a descriptor count nobody can know at startup, because
  other processes contribute to it — the workload that found #301 was at the floor by way of
  48 ranks × ~6 files it did not account for, read at 186 MB/s where 1293 was available, and
  had nothing in the log saying so. The runtime warning needs no prediction.

  The floor is a share of **2**, not a share below 2: integer division reaches exactly 2 one
  descriptor before the clamp starts applying. A cross-check against the divisor's own
  arithmetic caught that off-by-one, which would have reported every crossing one count late.

- **`lith_prefetch_committed_bytes` and `lith_prefetch_budget_bytes`: what the prefetch
  budget actually bounds** ([#301](https://github.com/scttfrdmn/lith/issues/301)).
  `--prefetch-budget` is about bytes prefetch has committed and nothing has consumed, and
  that quantity was nowhere observable — the budget was enforced by a **proxy**, the per-handle readahead
  window times the open-handle count, and the proxy was all anyone could measure. It is also
  the proxy that charges for idle file descriptors at a measured 6.38× wall-clock cost.

  The `prefetched` set now carries each chunk's byte length so the three removal paths
  (consumed, evicted unread, failed fill) can maintain an exact running total.

  **Deliberately not validated against the proxy.** The reporting workload tried the
  offline derivation first and found it circular: an estimator built from dispatch counts
  returns the standing window by construction — one establishment burst of exactly the
  window, then +1 per block boundary *including boundaries past EOF* — so
  `resident ≈ window × handles` held in 50/50 of their cells as an identity. They withdrew
  a draft conclusion that rested on it. The tests here assert against constructed state
  instead: 8 chunks prefetched, one consumed, re-prefetch not double-counted, drain to zero,
  and a short trailing chunk counted at its real length.

  **Measured on a real mount, the proxy over-charges by exactly the open-handle count.**
  Tightness is `1/N` to within 6.2% from N=1 to N=256: at 256 descriptors the mount is
  charged 4295 MB, holds **17.8 MB**, and is throttled ~9.5× for it, while even the tightest
  case (N=1, tightness 1.000) leaves the budget 55% empty. The error is linear in descriptor
  count, not a constant a fudge factor could absorb. The gauge also validated in that run —
  the budget reads 4.127829504e9 exactly, committed drains to **0 at unmount in 18/18 cells**
  under a real FUSE read loop, and at N=1 it steps 0 → 1870.659584 MB in one 100 ms sample,
  which is 223 × 8 MiB to the byte.

  Named **committed**, not resident: the counter increments at dispatch, so it includes bytes
  whose GET is still in flight. Right for admission control, wrong for a memory limit — the
  earlier name invited enforcing a RAM bound with it.

- **`lith_readahead_window_blocks` and `lith_open_handles`: the realized readahead depth
  and its divisor** ([#298](https://github.com/scttfrdmn/lith/issues/298)). The depth a
  handle actually gets is `clamp(--prefetch-budget / open-handles, 2, --max-readahead)`,
  and `open-handles` counts every open file **descriptor** on the mount — across processes,
  per descriptor rather than per object, and **including descriptors that have never been
  read**. So a job holding 256 files open drives its own and every other reader's readahead
  to the floor of **2 blocks** whatever the flag says.

  Measured by the reporting workload at **6.38× the wall clock** (186 MB/s where 1293 was
  available) on bytes and requests that differ by **0.4% and 3%** — the third and largest
  instance on #256 of "byte-identical is a true and insufficient regression gate", and
  invisible to every counter lith had. Both of that workload's production mounts were
  running at the floor, through no flag anyone set, which is also why `--max-readahead`
  looked inert through a dozen gates: it was never reachable from the workload.

  Both gauges are needed, because a window of 2 could be a tight budget or a crowded mount
  and only the divisor distinguishes them. The startup warning added in
  [#297](https://github.com/scttfrdmn/lith/issues/297) cannot cover this case — it is
  single-handle, and the divisor is dynamic.

  **Reporting only.** Whether the divisor should charge for idle descriptors is a policy
  question with a much larger blast radius, tracked separately.

### Fixed

- **The coverage-gate counters are live, and the one that matters now exists**
  ([#316](https://github.com/scttfrdmn/lith/issues/316)). Two defects in the counter added one
  day earlier, both found by an external 48-rank run reading the code:

  1. **It was folded in once per handle at `Release`, so it was useless on a running job.** It
     read 0 in every 2 Hz sample through the whole run and only appeared (527, 522) in the
     final scrape, after the job was killed and its handles closed. A workload that holds its
     handles open for the duration — the normal shape there — could never see the defect it
     exists to show. Now incremented per read.
  2. **The rejection that *is* #316 was counted nowhere.** `lowCoverage` increments only on the
     seek path; a handle making contiguous progress and refused a window takes a different
     branch, and `deEstablish()` counts only an establishment that existed — these handles are
     refused while still provisional. So the purest #316 cell incremented **zero** counters.

  New `lith_prefetch_coverage_held_total` covers it, and the pairing is the diagnostic: held
  rising while `low_coverage_total` stays flat is contiguous progress denied a window (#316),
  whereas both rising is a scattered walk and the gate working as designed. It ticks at **block
  boundaries**, not per read.

- **`--mem-cache` is documented as not bounding RSS**
  ([#314](https://github.com/scttfrdmn/lith/issues/314)). In-flight prefetch is not charged
  against the cache it is about to land in, so the footprint is the tier **plus** the
  outstanding burst — measured additive to 0.2%, and an OOM kill at 30.56 GB RSS for a 20 GB
  tier on a 33.0 GB box. Also corrected: `--max-readahead`'s help still described the
  byte-exact admission reverted in #309, and `docs/knobs.md` still said the #301 repair was
  pending when it had shipped. And `lith_prefetch_issued_total` is now documented as blind to
  thrash — evicted blocks return as uncovered demand reads, so
  `lith_prefetch_uncovered_total` is the counter that moves ([#313](https://github.com/scttfrdmn/lith/issues/313)).

- **The mount now reports the readahead window a handle will actually get**
  ([#297](https://github.com/scttfrdmn/lith/issues/297)). It logged the *configured* depth
  at a point where the effective depth was already different: `--block-size 8MiB
  --max-readahead 1024` printed `blocks=1024` and delivered **492**, with no warning. A
  readahead measurement at a non-default block size was therefore not the experiment it was
  configured to be, which is how it was found — the reporting workload only noticed because
  `--pf-trace` records `peak_window`.

  The cause is that `--prefetch-budget` is a **byte** budget and the window is counted in
  **blocks**, so `perHandleWindow()` divides one by the block size and the result can bind
  first. On a 33 GB box the budget is ~4.13 GB: **492 blocks at 8 MiB, 3936 at 1 MiB.** Which
  bound wins therefore changes with `--block-size`, which is a large behavioural difference
  arising from a unit conversion rather than a policy. The mount now warns with the
  configured depth, the effective depth and **which bound produced it**, taking the
  budget-in-blocks figure from `BlockStore.PrefetchBudgetBlocks` rather than re-deriving it.

  It also explains a null result that was otherwise puzzling: raising `--max-readahead` from
  223 to 492 at 8 MiB measured **+3%**, within noise, because both configurations were
  already against a ceiling neither of them set. The broader problem — three bounds on
  outstanding readahead, derived from three different quantities, disagreeing by 1.5× in the
  shipping default — is [#298](https://github.com/scttfrdmn/lith/issues/298).

### Changed

- **`--readahead-evidence-ratio` is documented as in-region-only, with the mechanism**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). At 58.6 ms RTT, on an object larger
  than the prefetch window, the flag costs a cleanly separated **2.56×** wall clock — median
  8.51 s to 21.74 s, zero overlap across n=8 per arm — while moving **identical bytes in
  identical request counts** (3,776,834,855 B and 465 GETs in every cell). In-region the same
  effect is **+7.5%**.

  The cause is one dispatch event. `advance(cursor+1+window)` emits its blocks in a single
  call, so the window value at establishment **is** the batch size, and
  [#229](https://github.com/scttfrdmn/lith/issues/229) exists to make that batch the full
  window — its comment naming the alternative as *"many small GETs, an underfed NIC on the cold
  read"*. The gate caps that batch, which is that regression reintroduced: with the gate off one
  read dispatches **223 blocks** and every later dispatch is **one** block, and that single
  burst is the entire throughput advantage. Capped to 41, 5 and 2 blocks at k = 40, 4 and 1, the
  read falls from 3.10 to 1.12 concurrent streams' worth — essentially serial.

  **It cannot be tuned away.** Concurrency cannot fall below one stream, so a 40× range in how
  long the cap binds buys only a 3.1× range in the penalty; the gate bounds committed readahead
  bytes and on a long pipe committed bytes *are* what buy concurrency, so the byte saving and
  the burst are the same quantity. `docs/knobs.md` now says so, and the analysis is in
  `bench/evidence-ratio/high-rtt/` with a script that reproduces it from published traces.

  Two corrections fall out. Earlier reports of a **bimodal** 4 s / 20 s split were an artefact
  of a test object *smaller* than the window, where readahead extent was never binding. And
  four mechanisms were proposed and refuted before this one — two of them ours — of which the
  last two were killed offline, from traces already published, at no cost.

### Fixed

- **Reverted: the evidence gate's window floor is a constant again**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). A floor derived from the
  device (NIC baseline x rolling TTFB median) briefly replaced the constant
  `initialWindow` of 2 blocks, on the reasoning that a window under one first-byte
  round trip cannot keep a reader fed and that this explained a bimodal cross-region
  wall-clock split. **Measurement refuted it and the change is backed out.**

  Raising the floor so that no window fell below a round trip left the stall rate
  **unchanged** — 4 of 8 cells over 9 s before and after — and the decisive evidence
  is the `--pf-trace` window series, which is **bit-identical between a 20.3 s cell
  and a 3.4 s cell of the same arm**: same 6432 windows, same min and max, same
  dispatch count. A 6x wall difference with an identical window trajectory means the
  window is not the channel.

  Two further reasons not to keep it. It was **not actually device-derived**:
  `RoundTripBytes` was read once at `Open()`, and `currentTTFB()` returns a hard-coded
  40 ms seed until a fill has recorded a sample, so a single-handle reader on a fresh
  mount — the ordinary case — always got the seed. The floor was therefore a constant
  **30 blocks at 2.2 ms and at 58.6 ms alike**, confirmed by the measured minimum
  window being exactly 30 at both endpoints. And it **cost bytes on the case the flag
  exists for**: a cold low-coverage read went from 59.8 MB to 210.8 MB of S3 traffic,
  turning a 95.1% byte saving into 82.7% — 13% of the win — because a 30-block floor
  commits 240 MiB to a read that wants 22.5 MB. The inertness test that was supposed
  to prevent that set the floor directly instead of going through the device path, so
  it validated the arithmetic while the wiring delivered a constant.


## [1.2.0] - 2026-09-30

**A minor, not a patch:** this adds two flags (`--pf-trace`,
`--readahead-evidence-ratio`, both off by default) and three metrics, which is new
backward-compatible functionality.

The substance is an investigation ([#256](https://github.com/scttfrdmn/lith/issues/256))
into when readahead over-fetches, conducted with an external workload, and the
corrections it forced. Two real read-path bugs are fixed: a byte gap computed
outside the decision lock could make a concurrently-read sequential handle look
like it was seeking and lose its readahead, and a just-prefetched chunk could be
discarded on arrival under memory pressure while the thrash metric that exists to
report exactly that had never been able to fire.

Three documentation claims turned out to be wrong when measured, and are corrected
here rather than quietly: the losing access shape is **low coverage**, not
"scattered reads" (same read size, monotone end to end, so the detector was
classifying it correctly all along); `--mem-cache` needs ~2.5x the distinct working
set rather than 1x, because the tier is sharded 64 ways; and the claim that
evidence-proportional readahead "does not improve precision" held only at 48 MPI
ranks — with a single reader it cuts over-fetch by up to 95% while follow-through
rises.

Also fixed: five earlier releases published with an **empty** body, and the
`lith-pfreplay` diagnostic (not a shipped binary) gained the offline replay,
shared-cache, key-level, granularity and eviction passes the investigation ran on.

**One caveat shipped knowingly.** `--readahead-evidence-ratio` is experimental and
off by default, and it should stay off for **high-latency or non-AWS endpoints**:
measured cross-region it leaves bytes and request counts identical but makes wall
clock bimodal, with `wall > 9 s` in 0 of 16 unclamped cells against 8 of 16 when
enabled. In-region it costs a fixed ~+0.35 s and no stall appears. The corollary is
the transferable part — a bytes-and-requests regression check is **not** sufficient
for this flag, because those are identical on both sides of the stall.

### Added

- **`lith-pfreplay -mem-cache`: drive the real memory tier, instead of assuming no
  eviction** ([#256](https://github.com/scttfrdmn/lith/issues/256)). Three gates reported
  their shared-cache numbers with the same caveat — *no eviction, therefore an upper bound
  on redemption* — and that caveat had become the binding limitation on the instrument
  rather than a footnote on one figure. `blockstore.CacheModel` drives the **production
  `memCache`**, sharded 64 ways as production shards it, so the replay cannot diverge from
  the policy it claims to model; it reports re-fetches of evicted chunks, prefetched chunks
  evicted before any read consumed them, and demand residency. A model of a 32 GB cache
  cannot allocate 32 GB, so every chunk handed to the tier is a sub-slice of one shared
  backing buffer — `len(data)` is the only thing the tier's size accounting reads, so it
  stays exact while the model stays O(chunk).

  **Driving the real tier rather than simulating it is the point, and it paid immediately.**
  A 2Q model written from Johnson & Shasha — or from this package's own type comment —
  would have been wrong twice: the tier never enforces its `Kin` bound
  ([#279](https://github.com/scttfrdmn/lith/issues/279)) and a freshly filled prefetch chunk
  is not yet flagged unread when eviction runs
  ([#280](https://github.com/scttfrdmn/lith/issues/280)). Such a model would have been both
  more scan-resistant *and* more protective of prefetch than lith actually is — optimistic
  on hit rate, in a tool whose entire purpose is to bound optimism, which is
  [#267](https://github.com/scttfrdmn/lith/issues/267)'s "a replay that does not know it
  runs a different program" one layer down.

  Three limits are stated in the source rather than left to be discovered: the chunk key
  omits bucket and ETag (statistically equivalent shard distribution, not identical), pins
  are not modelled (so it errs toward reporting eviction as *binding*, the safe direction),
  and the walk is decision order rather than wall clock (so redemption remains an upper
  bound — now bounded by capacity as well as by order). **Not yet run against the banked
  capture-2 traces**, so the tightness claim those three gates carried is still unverified
  on real data: the instrument exists and has not been pointed at it.

- **`lith-pfreplay -keys`: fit the #256 rule at the object level**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)), contributed as a patch by the
  reporting workload and taken as written. If 84–85% of redeemed prefetch bytes are read
  by a handle other than the one that fetched them, then follow-through is not a property
  of the handle — so the rule is fit per `(label, arm, key)` instead, against the
  cold-start tax as well as follow-through. Per-key aggregates come out of `globalScore`'s
  own loops (a fourth return value) so the dedup, the EOF clamp and the first-run-cold
  rule cannot drift between units. Two features qualify on two arms each where the
  handle-level fit had none: `distinct_frac` at **−0.689/−0.707** on the tax and
  `reads_per_reader` at **+0.615/+0.627** on follow-through. Post-hoc features are printed
  but **barred in code** from qualifying a feature or setting the best |ρ| the verdict
  reads off, and a within-class constant now prints `const(v)` rather than a bare `n/a`,
  because "this does not vary" is a fact about the workload and not a missing measurement.

  **The tax predictor survives the obvious objection.** `distinct_frac` (D/S) shares D
  with the target (W/D), so the sign could be induced. A permutation null (shuffle W, keep
  (D,S) paired, 2,000 draws) centres at **+0.171/+0.173**, 95% [+0.037, +0.303], p <
  0.0005 — re-derived here independently of the reporter's script, agreeing to within
  0.002. The null's own positive centre is **not** explained by ρ(D,W) as first reported:
  a permutation destroys that pairing, and re-attaching W to drive ρ(D,W) from +0.993 to
  −0.993 moves the centre by 0.002. It is the size/coverage structure
  (−ρ(`distinct_frac`, D) = +0.287).

  **It does not license an object-scoped hint, and that is the load-bearing result.** The
  floor is diffuse — the top 10% of objects hold **21.8%** of it, not the ≥50%
  pre-registered — and every rule that catches it (`coverage < 0.25`, `size > 8 MiB`)
  fires on **70–81% of the working set**, including on half of the well-behaved mount's
  objects. The direction that remains is to make the granularity commitment conditional
  on evidence rather than attaching hints to objects.

- **`lith-pfreplay -granularity`: price the cold-start fetch unit**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). The fix direction left standing
  is the granularity commitment at `internal/fuse/fs.go:778-786`, where a cold handle
  fetches a whole 1 MiB chunk for a 100 KiB read. Inverting it outright is not obviously
  right, because of a number in the shared accounting nobody had read: of the measured
  24,801 first-run cold small reads, **20,610 are suppressed** — they land in a chunk
  another handle already paid for and cost nothing. The chunk is the right unit 83% of the
  time by hit count and the wrong unit by byte count. So this sweeps the fetch unit
  instead of assuming a binary, charging each `(key, extent)` once mount-wide and counting
  the bytes no reader ever touches. The answer is a **knee, not a cliff**, priced in
  **requests** (one ranged GET per per-chunk fill, not one per extent — the first cut of
  this sweep conflated the two and overstated the cost 1.72×): half the floor for **1.61×**
  the cold GETs at 512 KiB, 75% for **2.77×** at 256 KiB, and full byte-exact serving for
  **5.65×** — measured on the reporting workload's published traces, against a ceiling of
  one GET per cold read (5.85×), so the ceiling is nearly tight. The exchange rate runs
  257 → 133 → 63 KiB bought per extra request. The well-behaved mount pays 722 extra
  requests to save 23 MiB, so a global shrink would tax the good reader to pay for the
  sparse one. **It proposes no default**; it prices the trade, offline, at no cost.

  The no-eviction caveat this originally carried is now **measured rather than inherited**:
  at the reporting workload's own 24 GB and 32 GB memory tiers, eviction is **not binding
  at all** — zero re-fetches, zero unread evictions — so the 83% free-hit rate that is the
  strongest argument against byte-exact serving stands unchanged for that configuration.
  It begins to bind at ~8 GB and costs 2.2 GB of re-fetch by 4 GB.

- **`lith-pfreplay -global`: the shared-cache accounting unit**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)), contributed as a patch by
  the reporting workload and taken essentially as written. lith's block cache is per
  **mount**, not per handle, so a second dispatch of a resident chunk costs nothing —
  but every verdict this tool had produced charged it anyway. `-global` re-scores the
  same replay against one cache per mount: a `(key, block)` pair is charged once, to
  whichever handle dispatched it first in decision order, and a dispatch counts as
  followed through if **any** handle on that key later reads it. Detectors stay
  per-handle, exactly as the live code does — the cache is shared, the state machines
  are not. Only possible because of `seq`, and it refuses to run without it rather
  than invent an order. Assumption stated in the file: no eviction, so it is an upper
  bound (tight on these traces at 3.0–3.4 GiB distinct against 24–32 GB of cache).

  **The unit is not a matter of taste — lith's own counter picks it.** Against
  `lith_prefetch_issued_total`, shared comes to **1.04–1.08×** what the mount actually
  fetched where per-handle is **1.50–4.56×**. It also explains why no single
  correction ever fit: met dedupes **4.2×** (12 keys, ~600 handles) and HEMCO
  **1.45×** (204 keys, ~6,200), so the 3.42× quoted for three gates was the average of
  two different mounts. And **84–85% of all redeemed bytes were read by a different
  handle than fetched them**, which is the real lesson: *"did this handle's prefetch
  pay off" is not a property of the handle.*

### Fixed

- **`lith-pfreplay` printed a 24 GB capacity as `23437500KiB`.** 24 GB is an exact multiple
  of 1 KiB, so the exact-multiple formatter chose a unit nobody reads. An exact binary
  multiple now has to be a sane magnitude as well as exact, falling back to one decimal
  place in the largest unit that fits.

- **Documentation said "scattered reads" where it meant "low coverage"**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). `docs/knobs.md`, this
  changelog's own #256 entries and the issue title all described the losing access shape
  in **read-size and read-order** terms. A key-level fit over two mounts of one workload
  shows that was the wrong column. The two mounts have **the same read size** (104 vs 122
  KiB median); every read on both is **sub-chunk** (the eligibility feature is a *constant*
  1.000 across all 216 objects, so it cannot distinguish anything); and both walk their
  objects **monotonically end to end** — meaning `sequential` was the **correct**
  classification and the detector was never fooled, which is the assumption four refuted
  hypotheses had rested on. Readers also **partition each object byte for byte** (zero
  measured overlap), so none of the waste is redundant reading. What separates the mounts
  is how much of each object is ever touched: **16% against 65%**. `docs/knobs.md` now
  states the correction and says to measure `union(bytes read) / object size` rather than
  read size when judging whether a reader will over-fetch.

- **A MiB/MB unit error in the reported cold-waste comparison**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). The shared-cache cold waste was
  reported as 1,257.3 MiB and described as closer to the live measurement (1,224 MB) than
  the reporter's own 1,318 MB figure. **1,257.3 MiB *is* 1,318.4 MB** — the same number
  against decimal-MB counters, so there was no improvement to report. The conditional
  `Open()` fix moves the cold-waste estimate by **zero** (the cold tax is counted on demand
  reads, not dispatches, so it could not have); what it fixes is fidelity, completely. The
  standing comparison is **1,318 MB replay vs 1,224 MB live = 7.7% high**.

- **`readTrace` never parsed the `key` column.** It has been in the trace format since
  [#262](https://github.com/scttfrdmn/lith/issues/262) and went unused through three
  gates — and since dedup is per object, the shared-cache unit was unbuildable until it
  was added. A column the format emits and the parser silently drops is the same class
  of defect as a metric that emits no series and a flag that reaches nothing.

- **`cmd/lith-pfreplay`: replay a `--pf-trace` and score per-handle byte
  follow-through offline** ([#256](https://github.com/scttfrdmn/lith/issues/256)).
  Four hypotheses for #256 each cost a ~35-minute 48-rank cluster job to refute.
  This replays a trace through the real `internal/prefetch` detector, recovers the
  blocks it dispatched, and scores what fraction of those **bytes** the same handle
  later read — so a candidate policy costs a replay instead of a run. Not a shipped
  binary (goreleaser builds only `./cmd/lith`).

  Dispatched blocks are **clamped at EOF** exactly as `store.Prefetch` clamps them:
  without that, a deep readahead window against a small object counts blocks that
  fetched nothing (measured at ~94% of the denominator in one case), dragging every
  score to ~0 and reading as "prefetch never pays off". The cold-start tax is
  measured **net** of bytes the same handle later reads, and first-run `cold` is
  split from re-entries — both corrections requested after the first version
  inflated a streaming arm's waste by hundreds of MiB.

  Fidelity is reported first and **labelled for what it does not prove**: the
  state-machine check compares `Observe` against a trace column that also came from
  `Observe`, so it cannot catch a byte-accounting error. A denominator sanity check
  and an optional `-issued` cross-check against the live counter are the independent
  ones. The trace gained a `size` column so the clamp is exact rather than estimated.
  The scoring rule is the one agreed and pre-registered with the reporting workload
  before either side had data — bytes not chunk touches, Spearman |ρ| ≥ 0.5 fit on
  one arm and tested on another, no-separation iff |ρ| < 0.5 **and** rank-sum
  AUC < 0.7 — so it cannot be tuned to the answer. It also counts the #256
  cold-start granularity tax directly from the trace.

- **`--pf-trace`: the access-pattern detector's decisions, recorded and replayable**
  ([#262](https://github.com/scttfrdmn/lith/issues/262)). The trace behind
  `LITH_PF_TRACE` has existed for a while but could not answer what it was needed
  for, because rows carried the object key and **not the file handle** — lith builds
  one prefetcher per `open`, so the handle is the unit that makes decisions, and
  concurrent handles on one object interleaved indistinguishably (dozens of them
  under a many-rank job through one daemon). Rows now carry `fh` and `pid`.

  It was also a **silently biased sample**: reads served while a whole-file parts
  fetch was in flight, or by a footer handle's byte-exact plan, were dropped with no
  marker — and those are a large, non-random share of the bytes wherever
  [#229](https://github.com/scttfrdmn/lith/issues/229) whole-fetches large objects.
  A test case here drops **72%** of its reads under the old behaviour. Every read is
  now recorded with a `path` column (`window` / `parts` / `footer`) saying which
  path served it.

  Rows gained `window` and `dispatched`, and the file gained a `#` header line
  recording the config that produced it (block size, max-readahead, parts-max,
  small-file, coverage gate, evidence ratio), so a trace is self-describing,
  comparable across runs, and an offline replay can be **validated for fidelity**
  before its verdict on a new policy is trusted. A failed trace-file create is now
  logged instead of silently yielding an empty trace and a successful-looking run.
  Documented in `docs/knobs.md`, with its cost stated from measurement (see Fixed
  below): the mutex costs +0.5-1.6% wall at 48 MPI ranks over ~60k traced reads, so
  the trace file's size is the real constraint rather than the lock.


- **`--readahead-evidence-ratio`, experimental and off by default**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). Bounds a committed
  readahead window to a multiple of the bytes a handle has actually read. A handle
  establishes at its first **block crossing** — one block (8 MiB) of contiguous
  evidence — and today that buys the full NIC-derived window, ~223 blocks
  (~1.8 GB) on a 50 Gbps node. With a ratio of `k` a handle prefetches ~`k`× what
  it has read, so a sequential copy still earns the full window (once it has
  consumed `max-readahead × block-size ÷ k`) while a reader that tiles a slab and
  jumps does not.

  **What it does and does not do, measured on the reporting workload** (GCHP
  fullchem, 48 ranks, PR binary with the flag unset as the control): it cuts
  amplification **2.425× → 1.956×** at `k=8`, which is within 1.5% of what pinning
  `--max-readahead 1` achieves (1.925×) — but it gets there *without* pinning a
  global window, so a sequential reader can still earn the full one. It does
  **not** improve prefetch precision: the hit rate went **16.7% → 14.3%**, because
  accrued evidence turns out to be uncorrelated with whether a prefetch is used.
  So this is an ergonomics and safety improvement over telling operators to pin
  `--max-readahead`, and **not** an answer to #256's precision question, which
  stays open. Off by default; with the flag unset the read path is unchanged
  (reproduced to 0.5% on the cluster).
- **`lith_prefetch_deestablished_total{size_class}`**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)) counts how many times a
  handle lost an establishment it had. It is **not**
  `lith_prefetch_reset_random_total`, which counts collapses to the Random *state*
  and is 20–30× larger (307–332 per run against 10–17 on the same mount) — a
  distinction that matters, because conflating them made a re-establishment cap
  look worth building and it measured completely inert. Kept because the counter
  separates mounts in the *opposite* direction to the obvious guess: the mount with
  an 80% prefetch hit rate de-establishes **51** times (45% of its Random
  collapses) while the pathological one de-establishes **10** (3%). Frequent
  re-anchoring marks a reader whose prefetch works, not one whose prefetch is
  wasted.

- **`lith_prefetch_evidence_clamped_total{size_class}` and
  `lith_prefetch_evidence_withheld_blocks_total`** make the gate's action
  observable. Without them the only visible effect was that `prefetch_issued`
  fell, which cannot distinguish a window the gate refused from one the detector
  never wanted — and cannot answer whether the gate fires on the large objects or
  the small ones.


- **`lith-pfreplay` now reproduces the mount's conditional `Open()`, closing the last
  source of replay divergence.** The mount calls `pf.open()` **only** for objects
  larger than `partsThreshold`; for a smaller object the prefetcher is never
  `Open()`ed, so its first `Observe` takes the `!haveLast` path — `lastBlock =
  blockIdx` rather than `-1`, leaving `lastDelta` at 0 and making the strided branch
  unreachable on read 2. Replaying `Open()` unconditionally dispatched a block the
  mount did not, and **only** on handles that never reach sequential or strided —
  exactly the population a field report isolated (10 of 6,229 HEMCO handles, every
  row `cold` or `random`, median object 33.4 MB against a 64 MiB `parts-max`). With
  it, all four real traces replay at **fidelity OK on every handle** and replay
  decisions equal the mount's own column exactly (52,104 = 52,104). The pre-registered
  verdict is unchanged, so the finding never depended on the defect.
- **A mismatch now names its first diverging row** (`fh`, `seq`, block, offset, gap,
  `max_window`, state transition, mount-vs-replay dispatch counts). Counting
  mismatches says a replay is wrong; naming the row is what made the cause findable.

- **`lith-pfreplay` replays in decision order, not file order**
  ([#272](https://github.com/scttfrdmn/lith/issues/272)). #271 recorded a monotonic
  `seq` inside the handle's lock, and the replay parsed it and then sorted by nothing
  — so the ordering fix stopped short of the thing it was for. Measured on live
  mounts: **10.9%** of rows arrive out of decision order with 256 handles, and
  **0.0%** with a single reader, which is why my own verification (single-threaded,
  so file order *was* decision order) reported a clean 1.00× round-trip while the
  defect was present. Now sorted per handle by `seq`, with
  `TestReplayIsInvariantToRowOrder` shuffling a known-good trace and requiring
  identical results — a property the tool should hold unconditionally.

- **The replay's verdict now respects its own fidelity gate**
  ([#267](https://github.com/scttfrdmn/lith/pull/267)). On the real 48-rank capture
  the tool printed *"everything below is void"* from the fidelity gate and then,
  eleven lines later, **`VERDICT: SEPARATION`** — disagreeing with itself on one page.
  `--min-n` did not help, because it counts *scored* handles and a diverged handle is
  scored, just scored wrong. The verdict is now computed on **faithful handles only**,
  the exclusion is stated, and a class left below `--min-n` (including zero) yields
  **UNEVALUABLE** rather than "no separation" — an absence of data is not a measured
  absence of relationship.
- **The trace records `max_window` and a decision `seq`**
  ([#267](https://github.com/scttfrdmn/lith/pull/267)). The live path calls
  `SetMax(perHandleWindow())` before **every** `Observe`, and that input is mount-wide
  and time-varying; the replay ran the static cap instead, dispatching **2.70×** what
  the mount did on a capture where the mount never exceeded 17 blocks and the replay
  assumed 223. And rows were appended outside the handle's lock, so **1.9–2.0%** of
  window rows were out of decision order — some logically impossible. `seq` is now
  allocated *inside* the lock (a timestamp in the writer could not fix this: it would
  stamp the append, recording the wrong order faithfully), and `after`/`window`/`peak`
  are captured there too, so they can no longer describe a different read's transition.
- **The replay-vs-mount gap is now decomposed instead of attributed.** A single ratio
  blamed dedup for a replay-input error: a 9.25× gap read as ~13× sharing when it was
  **2.70× window inflation × 3.43× genuine dedup**. The mount's own `dispatched`
  column is the pivot that separates them and needs no new column.

- **An unwritable `--pf-trace` path now fails the mount instead of mounting
  successfully without a trace** ([#264](https://github.com/scttfrdmn/lith/issues/264)).
  Under `--daemon` the error went to `/tmp/lith-<uid>-mount.log` and the mount came
  up fine, so a whole characterization job could run and yield nothing —
  non-fatally and invisibly, in exactly the mode a capture uses. The path is now
  validated in the foreground, before the daemon fork. A diagnostic the operator
  asked for by name is not best-effort.
- **`--pf-trace`'s cost is now stated from measurement, not caution.** The help and
  `docs/knobs.md` warned that its mutex "adds a global lock to every read: for
  characterization, not production", which read as unusable under load — a reporter
  nearly designed a two-job capture around it. Measured at **48 MPI ranks over ~60k
  traced reads: +0.5–1.6% wall**, with the traced run reproducing an untraced one on
  every axis. Now says the lock is negligible below roughly 10⁵ reads/run and points
  at the file size instead.

- **Three more defects in `lith-pfreplay`, all found before the expensive capture**
  ([#267](https://github.com/scttfrdmn/lith/pull/267)). A **degenerate arm could
  declare `VERDICT: SEPARATION`** — n=3 with a two-way tie on both axes is monotone
  by construction, and the winning feature was a restatement of which handle had
  enough rows to be scored at all; correlations now count toward the verdict only
  from arms with enough handles and enough distinct values on both axes, and
  replication across arms no longer excuses a degenerate fit. The **`-issued`
  cross-check compared one run's counter against each trace separately**, so the
  line that exists to catch a 12× error was itself off by up to N×; it is now
  computed once over the summed replay, and reports the factor by which per-handle
  scores understate prefetch's value. And when a trace has no `size` column the
  **denominator-sanity ratio is suppressed rather than printed**, because estimated
  per-handle sizes inflate "distinct objects" while decisions collapse — measured
  turning 2.02× into 0.07×, which reads as reassuring at exactly the moment it is
  broken. A size-less trace also silently drops the handles that stopped early (the
  most wasteful ones), which is now reported.

- **Two compiled binaries are no longer tracked in git.** `go build ./cmd/<x>`
  writes its binary into the working directory, so building from the repo root
  leaves an executable there and `git add -A` commits it. `lith-s3bench` (14.1 MB)
  went in with #40/#48 and `lith-pfreplay` (2.8 MB) with #267 — together about
  **17 MB of a 42 MB pack**, and invisible at review because a binary appears in a
  diff as one unremarkable line. Both untracked, both added to `.gitignore`, and
  `TestNoBuiltBinariesTracked` now fails when a file named after a `cmd/` package is
  tracked — because `.gitignore` does nothing for an already-tracked path, which is
  how the second one got in while the first sat there. History still contains the
  blobs; rewriting it for this is not worth the disruption.

- **`--pf-trace` was accepted, documented, and silently ignored**
  ([#264](https://github.com/scttfrdmn/lith/issues/264)). The flag bound a field
  that never reached `fuse.Config`, so it produced no trace, no error, and exit 0 —
  and the docs guard passed because the flag *was* documented. Now wired, with a
  new guard (`TestEveryFlagBindingIsConsumed`) that walks the AST for flag
  registrations and fails when a bound field is never read anywhere else in the
  package. It catches exactly this: reverting the one-line fix makes it name
  `--pf-trace`.
- **`lith_prefetch_used_total` now says it is a chunk-touch count, not a byte
  count** ([#256](https://github.com/scttfrdmn/lith/issues/256)). A 64 KiB demand
  read marks the whole 1 MiB chunk *used*, so `used/issued` **overstates byte
  follow-through — and overstates it most for the scattered readers where prefetch
  is least useful.** A measured case reported **89%** by this ratio while at most
  **~25%** of the prefetched bytes were ever read. Every prefetch-efficiency number
  in the #256 campaign, including the ones used as kill conditions, was scored on
  this friendlier unit. The metric help and `docs/knobs.md` now say so, and point at
  `lith_fill_bytes_total{kind=...}` and `--pf-trace` for byte-level answers. No
  behaviour change — the counter is unchanged, its description was wrong.
- **Labelled prefetch counters now emit their series at zero**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)).
  `lith_prefetch_evidence_clamped_total` was only created when it fired, so at zero
  it emitted **no series at all** — indistinguishable in a scrape from "the binary
  lacks the feature", "a different label value", or "the mount was never opened". A
  reporter had to infer a zero from an absent line and could only confirm it because
  another arm proved the same binary did emit the series when it fired. This is the
  same present-and-zero-versus-absent trap as
  [#253](https://github.com/scttfrdmn/lith/issues/253), in a metric added to *fix*
  an observability complaint, so it now has a test.


- **GitHub releases carry release notes again — on the second attempt.**
  `.goreleaser.yaml` disabled goreleaser's git-log changelog (this hand-written file
  is the source of truth) but nothing supplied notes in its place, so every release
  from **v0.5.0** on (`v0.5.0`, `v1.1.0`–`v1.1.3`) published with an **empty body**.
  The release workflow now extracts the tag's section from this file
  (`scripts/release-notes.sh`) and passes it to goreleaser as `--release-notes`.

  **The first fix did not work, and the rehearsal tag is what found it.** Adding
  `--release-notes` while *keeping* `changelog: disable: true` leaves the two
  fighting: with the changelog disabled goreleaser short-circuits before applying
  the notes, and `v1.2.0-rc.1` published a **2-byte body** (`"\n\n"`) alongside a
  complete and correct 16-asset set. The `disable` key is now gone — supplying notes
  already skips generation, which is all disabling was trying to achieve.

  The post-publish check is also stronger than it was, because the original was too
  weak in a way this exposed from the other side: asserting the body is merely
  **non-empty** cannot distinguish the CHANGELOG section from a git-log changelog, so
  a fallback to generation would have passed while shipping the wrong body. It now
  asserts the published body **contains the notes that were supplied**. The five affected releases were backfilled from
  this file, so every published release now carries its notes.
- **Two stale instrumentation strings that misled a bug reporter**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). `--parts-max`'s help still
  said files are fetched whole **"on first read"**, which has been untrue since
  [#229](https://github.com/scttfrdmn/lith/issues/229) gated the parts fetch on the
  handle's reads proving they tile — it reads as contradicting the documented
  behaviour, and cost a reporter a round trip to resolve. And
  `lith_prefetch_issued_total`/`_used_total` described themselves as counting
  **blocks** when they are recorded per **1 MiB chunk**, so a reader converting the
  counter to bytes had to guess the unit. Both corrected; no behaviour change.

## [1.1.3] - 2026-09-17

A gateway-observability fix from the GCHP shared-gateway validation
([#244](https://github.com/scttfrdmn/lith/issues/244)): the gateway's
distinct-bytes counter now advances, so a shared gateway's byte-dedup
amplification is computable from its own metrics endpoint.

### Fixed

- **`lith_distinct_bytes_read` now advances in `serve nfs` mode**
  ([#253](https://github.com/scttfrdmn/lith/issues/253)). The gateway read path
  never called `MarkDistinctRead`, so the counter stayed 0 — and because it was
  present-and-zero, the gateway amplification ratio `s3_bytes / distinct_bytes`
  read as a *perfect* result for an unmeasured gateway rather than failing
  visibly. The NFS read path now accounts distinct object bytes the same way the
  FUSE path does, so gateway amplification is computable from the gateway's own
  metrics endpoint (the counter [#250](https://github.com/scttfrdmn/lith/issues/250)
  needs).

## [1.1.2] - 2026-09-17

The concurrency fix from the GCHP project's shared-gateway test
([#244](https://github.com/scttfrdmn/lith/issues/244)) — `serve nfs` no longer
returns spurious `NFS3ERR_STALE` under concurrent readers — plus the enhancements
and observability from its end-to-end integration run
([#210](https://github.com/scttfrdmn/lith/issues/210)), the first workload to run
entirely off lith-served input.

### Added

- **`lith` logs a lookup miss (ENOENT) with the path**, at INFO, once per distinct
  path and capped ([#240](https://github.com/scttfrdmn/lith/issues/240)). A
  prefix-scoped mount that is silently short a key now says which one, instead of
  leaving the application's "file not found" as the only clue.

### Changed

- **`lith doctor` warns when `--nic-gbps` looks like the advertised peak**
  ([#239](https://github.com/scttfrdmn/lith/issues/239)). `--nic-gbps` wants the
  sustained baseline; the "Up to N Gigabit" figure on the instance page is the
  peak, and passing it over-sizes readahead and fetches bytes that are never
  read. `doctor` re-detects the true baseline and flags a peak-valued override.
- **`docs/knobs.md`**: `--mem-cache` is per-daemon (N mounts on a host add up;
  set it explicitly), and `--nic-gbps` is the baseline, not the "Up to N" peak.
- **`lith_nfs_ops_total` now counts the true NFS procedure**
  ([#247](https://github.com/scttfrdmn/lith/issues/247)). It previously counted at
  the billy-filesystem layer, which cannot see the procedure — so `getattr`
  silently included LOOKUP and ACCESS (all call `Lstat`), `readdirplus` included
  READDIR, and FSSTAT was uncounted. The gateway now counts per procedure via
  go-nfs's request-trace seam, yielding distinct `getattr`, `lookup`, `access`,
  `read`, `readdir`, `readdirplus`, `fsstat`, `fsinfo`, `pathconf`, `commit`,
  `readlink` (and an `unparsed` fallback so a future log-format change is visible,
  not silent). go-nfs's own error/warn logs now route through lith's slog and
  respect `--log-level`. **Watch for label changes** if you dashboard these:
  `getattr` no longer includes lookups/accesses.

### Fixed

- **`lith serve nfs` returned `NFS3ERR_STALE` on GETATTR under concurrent
  readers** (clean to ~32, ~15% from 48 up), which could abort a tightly-coupled
  MPI job at file open even though the bytes were never wrong
  ([#244](https://github.com/scttfrdmn/lith/issues/244)). The root cause was
  upstream in `go-nfs-client`'s `xdr.ReadOpaque`, which issued a single `r.Read`
  and ignored short reads: a file handle that straddled a buffer/TCP-segment
  boundary under load decoded with a zeroed tail, so `FromHandle` saw a bad inode
  (or root id) and correctly returned STALE. It was GETATTR-specific because that
  request path decodes the handle through the hand-rolled `ReadOpaque`, whereas
  READ decodes via `io.ReadFull`-backed struct unmarshaling and was never
  affected. Fixed by bumping `go-nfs-client` to the version whose `ReadOpaque`
  uses `io.ReadFull`. lith's own handle logic was never the bug — it round-trips
  `-race`-clean at 96×200 concurrent handles (`TestHandleRoundTripConcurrent`).
  The categorized STALE logging and the concurrency-ceiling note added while
  diagnosing this ([#245](https://github.com/scttfrdmn/lith/issues/245)) stay in.

## [1.1.1] - 2026-09-17

A patch for a real deployment finding from the GCHP project running 1.1.0 on AWS
ParallelCluster ([#237](https://github.com/scttfrdmn/lith/issues/237)). On stock
ParallelCluster nodes **both** NIC-detection paths fail — ENA reports no
`ethtool` speed, and the generated node role omits `ec2:DescribeInstanceTypes` —
and the failure fed a literal `0` into the device-derived knobs. `--parts-max`
silently clamped to its 4 MiB floor (a 16× swing off the 64 MiB it should be),
disabling the whole-file parts path for every 4–64 MiB file — exactly the band
where per-variable scientific inputs live — and `doctor` reported it healthy.

### Fixed

- **NIC detection never propagates `0` into the derived knobs**
  ([#237](https://github.com/scttfrdmn/lith/issues/237)). When every detection
  path fails, `--parts-max`, `--coalesce-gap` and `--inflight-bytes` now derive
  from a single assumed bandwidth (10 Gbps) instead of clamping to their floors.
  The whole-file parts path stays on for mid-size files on nodes lith cannot
  probe.
- **`lith doctor` prints the device-derived values `mount` will actually use**
  (parts-max, inflight, NIC source) and **`WARN`s — not `INFO`s — when the NIC
  is undetected**, naming the consequence and pointing at `--nic-gbps`. It no
  longer reports a healthy `PASS` over a 16× misconfiguration, or an
  inflight-bytes figure that contradicts the mount.

### Added

- **IMDS instance-type NIC estimate** ([#237](https://github.com/scttfrdmn/lith/issues/237)).
  When `ec2:DescribeInstanceTypes` is denied but IMDS is reachable (the
  ParallelCluster case), lith estimates the baseline from the instance size — no
  IAM change required. `--nic-gbps` remains the precise override, and
  `DescribeInstanceTypes` the precise detected path when permitted.

## [1.1.0] - 2026-09-16

The read path learned to **not commit before it knows**. The headline is one
fetch policy ([#229](https://github.com/scttfrdmn/lith/issues/229)): lith no
longer fetches broadly — whole-file parts, wide readahead — until a handle's
reads prove they tile. A sequential reader establishes in the first couple of
reads and gets the whole-file fetch as before; a scattered or sub-file reader (an
HDF5 hyperslab, a COG window, a GRIB `.idx` field sweep) stays byte-exact and
stops pulling whole objects to answer a question about a slice. Plus the release
now ships packaged and signed, and the prefetch detector stopped mistaking
scattered metadata walks for streams.

### Added

- **deb and rpm packages, an SBOM, cosign signatures, and SLSA build
  provenance** on every release (M15,
  [#207](https://github.com/scttfrdmn/lith/issues/207)). Packages (via goreleaser
  `nfpms`) and a syft SBOM attach to the release; checksums are signed keyless
  with cosign (Sigstore bundle); binaries and the image carry
  `actions/attest-build-provenance` attestations. `cosign verify-blob` and
  `dpkg -i`/`rpm -i` on a box, verified on the v1.0.2-rc.
- **The release gate is now the full branch gate** — vet, golangci-lint,
  `-race`, govulncheck, fuzz, dual-arch build, docker build — via one reusable
  workflow, no longer a subset ([#199](https://github.com/scttfrdmn/lith/issues/199)).

### Fixed

- **Release workflow pins goreleaser to the triggering tag**
  (`GORELEASER_CURRENT_TAG`) instead of `git describe`. An `-rc` and its stable
  tag legitimately share a commit; `git describe` then resolved ambiguously,
  so the stable run rebuilt the prerelease. Caught cutting this very release —
  the rc-then-real-tag verification did its job.

### Changed

- **Fetch policy: no broad fetch until the access pattern establishes**
  ([#229](https://github.com/scttfrdmn/lith/issues/229), subsuming
  [#220](https://github.com/scttfrdmn/lith/issues/220)/[#221](https://github.com/scttfrdmn/lith/issues/221)/[#228](https://github.com/scttfrdmn/lith/issues/228)).
  One signal — coverage over a trailing read window — gates the open-time
  parts-fetch, the initial readahead ramp, and the post-seek re-anchor. Measured
  amplification on scattered/sub-file reads (cold, parts-fetch on): HDF5
  hyperslab **92.5× → 6.3×**, COG overview+window **22.1× → 1.27×**, GRIB `.idx`
  **8.1× → 3.35×**; streaming and the CargoShip tree walk stay byte-identical.
  Cost: a cold sequential copy of a mid-size file pays roughly one extra
  round-trip for its first block (byte-identical, cold-only, shrinks with size).
- **Prefetch detector no longer mistakes a scattered metadata walk for a stream**
  ([#210](https://github.com/scttfrdmn/lith/issues/210)/[#213](https://github.com/scttfrdmn/lith/issues/213)).
  Reads that land in adjacent blocks but jump in byte offset — an HDF5/netCDF-4
  metadata traversal — were classified Sequential and got whole-block readahead;
  gap-aware then coverage-gated classification cut over-read **72–92%** on the
  GEOS-Chem gcgrid workload, ledger-confirmed.
- **`--parts-max` default is now `auto`** — derived from NIC baseline ×
  first-byte latency, clamped to `[--small-file, 64MiB]`
  ([#220](https://github.com/scttfrdmn/lith/issues/220)).

### Known issues

- **`mmap` of a large object walked randomly is slower than v1.0.1**
  ([#232](https://github.com/scttfrdmn/lith/issues/232)). The byte-exact posture
  that wins on scattered *reads* maximizes round-trips on serial *page faults*: a
  3,000-fault synthetic over an 892 MB index measured ~4.7× v1.0.1's wall
  (real-workload magnitude, e.g. `bwa`, unmeasured). Each random fault is a
  synchronous S3 round-trip and no readahead helps. **Mitigation:** copy a
  randomly-`mmap`'d reference index to local NVMe, or use the gateway / EFS — see
  `docs/copy-or-mount.md`.

## [1.0.1] - 2026-09-14

Hardening release from an external review of the v1.0.0 tag. No features. The
headline is a correctness fix: **`lith refresh` could return stale data** — a
failing-first integration test through a real FUSE mount confirmed it before the
fix. If you use `lith refresh`, upgrade.

### Fixed

- **`lith refresh` did not invalidate the kernel cache, so a reopen after a
  refresh could return the OLD version's size and bytes**
  ([#193](https://github.com/scttfrdmn/lith/issues/193)). The swap replaced an
  in-memory pointer, but the mount's one-year attribute/entry timeouts and
  `FOPEN_KEEP_CACHE` meant the kernel served a reopen from its cached
  previous-version attrs and pages — userspace never saw the read. **A
  failing-first integration test through a real mount confirmed the bug** (a
  reopen returned 4 KiB of the old content when the new version was 8 KiB of
  new content). The fix drives `notifyInvalInode`/`notifyInvalEntry` from a diff
  of the old and new index on swap; files unchanged between versions stay
  cached. (Verified by the same test passing post-fix, on a real kernel.)
- **`lith doctor` bypassed the production S3 client**
  ([#194](https://github.com/scttfrdmn/lith/issues/194)): it could sign a
  request to a non-HTTPS `--endpoint` — the exact case the mount path refuses —
  and so could report success on a configuration the real client rejects. doctor
  now builds its client through the same factory `mount` uses: a signed
  non-HTTPS endpoint is a check failure with the factory's own message,
  `--requester-pays` reaches the bucket and LIST probes, and a LIST denial is a
  **failure** (N/A only when `--keys` / `--keys-from-manifest` / `--cargoship` /
  `@ref` declares an alternate source).
- **The `CURRENT` pointer read was not bounded at the network**
  ([#195](https://github.com/scttfrdmn/lith/issues/195)): it read the whole
  object into memory before the 64 KiB cap applied. It now requests
  `MaxPointerBytes+1` and refuses an oversized pointer, naming the key and the
  cap — bounded in transit, not after the fact.
- **NFS gateway handle identity**
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): `--cargoship` and
  auto-list now derive the handle root id from the index's own content, like
  `@version` and `--index-file` already did, so a rebuilt namespace at the same
  location returns `NFS3ERR_STALE` instead of silently reinterpreting old handles.

### Changed

- **NFS gateway `seqState` is bounded** (LRU, 8192 entries) with a
  `lith_nfs_seq_states` gauge — it was an unbounded per-path map that leaked over
  a long-lived gateway ([#197](https://github.com/scttfrdmn/lith/issues/197)).
  Eviction only resets one file's readahead, never correctness.
- **The gateway readahead window is described honestly** — in code and docs — as
  a **mount-registration-based share** of the global budget, not a measured
  per-client fair share (go-nfs's read path carries no per-op client identity)
  ([#197](https://github.com/scttfrdmn/lith/issues/197)).
- **Removed the cosmetic `--no-portmap` flag** from `serve nfs`
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): only its default
  behavior ever existed. The gateway never registers with rpcbind; clients mount
  with an explicit port. No behavior change.
- **The 1.x index compatibility contract is published** on the
  [Scope page](https://scttfrdmn.github.io/lith/scope/) and enforced by golden
  fixtures ([#196](https://github.com/scttfrdmn/lith/issues/196)): every lith 1.x
  reader reads every index produced by lith 1.0.x. Stale "no compatibility
  promise before v1" language removed from the source, CONTRIBUTING, and design.
- **Documentation sweep** ([#198](https://github.com/scttfrdmn/lith/issues/198)):
  the README lists `serve`/`refresh`/`doctor` and the cluster/container/published/
  CargoShip pages and drops "Linux only (for v0.x)"; SECURITY.md drops "pre-1.0".
  The drift guard is extended beyond flags: CI now also fails on a command
  missing from the README summary, pre-1.0 status language, or a stale pre-1.0
  image tag / URL in any user-facing doc.

### Security

- **`lith doctor` no longer signs requests to a non-HTTPS endpoint**
  ([#194](https://github.com/scttfrdmn/lith/issues/194)) — the diagnostic path
  now enforces the same cleartext-credential guard as the mount path.
- **The NFS gateway's security boundary is stated plainly**
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): it is the network
  perimeter. The gateway advertises `AUTH_UNIX`/`AUTH_NULL` and does not verify
  client identity, so any host that can reach its port has read access — restrict
  it with a security group / trusted subnet. Now documented in SECURITY.md and
  the cluster page.

## [1.0.0] - 2026-09-14

**lith 1.0 — feature-complete for its thesis.** lith is read-only by definition
and cloud-native by thesis: it presents any S3 bucket as a read-only filesystem
in its native key layout, serving metadata locally at zero S3 operations, on one
node (FUSE) or across a cluster (the NFS gateway). With 1.0 the capabilities are
complete *for that thesis* — any POSIX tool reads any bucket, native layout or a
CargoShip/published dataset, with format-aware I/O, and a stranger reaches a
working mount in five minutes. Past 1.0, work is extension, not completion; what
lith does and deliberately does not do is published on the
**[Scope page](https://scttfrdmn.github.io/lith/scope/)**. This release is the
operability-and-honesty pass on top of the v0.5.0 feature set: first-run
diagnostics, health endpoints, a container image, and docs that provably match
the binary.

### Added

- **`lith doctor`** ([#176](https://github.com/scttfrdmn/lith/issues/176)):
  first-run diagnostics — eight checks (credentials, bucket + region, `LIST`
  permission, FUSE, mountpoint, NIC, the `@current` pointer, the index file),
  each `PASS`/`FAIL`/`N/A` with a one-line fix, non-zero exit on any failure,
  run through lith's own credential and endpoint resolution so "doctor OK" means
  "mount works." Verified by induction — every check broken on purpose and
  confirmed caught, which is how the `GetBucketRegion` error-wrapping
  misclassification was found and fixed before shipping.
- **Health endpoints — `/healthz` and `/readyz`**
  ([#177](https://github.com/scttfrdmn/lith/issues/177)): liveness and readiness
  on the metrics server. The gateway starts the health server *before* the index
  load, so `/readyz` returns `503` with a reason until it is serving and `200`
  after — wire both to an orchestrator.
- **Distroless container image — `ghcr.io/scttfrdmn/lith`**
  ([#178](https://github.com/scttfrdmn/lith/issues/178)): multi-arch
  (`linux/amd64` + `linux/arm64`), built by the release workflow from the *same*
  binaries as the tarballs and published to GHCR in the same gated run — no
  second build path. The image is for the gateway (`serve nfs` is a userspace
  server needing no privileges); `doctor` and `index build` run in it too. See
  [Running in a container](https://scttfrdmn.github.io/lith/running-in-a-container/).
- **Flag-documentation CI guard — `TestKnobsDocumentsEveryFlag`**
  ([#179](https://github.com/scttfrdmn/lith/issues/179)): fails CI if any command
  flag is missing from `docs/knobs.md`, so the documentation cannot drift from
  the binary.
- **[Scope page](https://scttfrdmn.github.io/lith/scope/)**
  ([#182](https://github.com/scttfrdmn/lith/issues/182)): what lith is for, the
  two-category boundary (writes are out *by definition*; everything else is
  deferred, each with the named signal that reopens it), and where lith is the
  wrong tool.

### Changed

- `docs/knobs.md` now documents every shipped flag; the `--footer-tier2` help
  is reconciled to the measured truth (experimental, clustered-projection only)
  ([#179](https://github.com/scttfrdmn/lith/issues/179)).
- The five-minute Start-here walk is re-verified end-to-end on a clean box and
  gains `lith doctor` as its pre-flight step
  ([#180](https://github.com/scttfrdmn/lith/issues/180)).
- `docs/not.md` is folded into the Scope page; the published `/not/` URL now
  redirects there.

### Removed

- Dead code ([#45](https://github.com/scttfrdmn/lith/issues/45)/[#46](https://github.com/scttfrdmn/lith/issues/46)):
  the unused `--disk-path` bench flag, a `benchBucket` global, an unused return
  value, a duplicate `*metrics.Metrics` handle, and Go-1.22 loop-variable copies.
  (`s3client.GetRange` was kept — it has production callers via pointer
  resolution — and documented, [#44](https://github.com/scttfrdmn/lith/issues/44).)

### Upgrade

- **No index rebuild required.** The index format is unchanged since v0.5.0;
  existing `--index-file` artifacts and published datasets mount as-is. lith
  reads **Linux only** — there are no macOS or Windows builds — and remains
  read-only: every mutating operation returns `EROFS`.

## [0.5.0] - 2026-09-14

**Published datasets: pack and publish once, mount it anywhere by name.** A
CargoShip-published dataset is a versioned prefix with an atomic `CURRENT`
pointer; `lith mount s3://bucket/dataset@current` resolves the pointer and mounts
the current immutable version — no index file to track, and two uncoordinated
readers of the same name serve byte-identical namespaces. This is the seam
between CargoShip and lith closed on lith's side.

### Added

- **Pointer-based mounts — `s3://bucket/dataset@current` / `@<version-id>`**
  ([#167](https://github.com/scttfrdmn/lith/issues/167)/[#169](https://github.com/scttfrdmn/lith/issues/169)):
  resolve the atomic `CURRENT` pointer to a published version's prebuilt index and
  mount it; a bare `s3://bucket/prefix` (no `@`) is unchanged. The `CURRENT` parser
  is hardened (#101 discipline: 64 KiB cap checked before decode, every field
  validated, unknown-fields/trailing rejected, never panics; fuzzed from a real
  pointer) and **fails closed** (#141): a missing, malformed, or dangling pointer —
  or one whose index came from a chunkless (unmountable) manifest — is a hard error
  naming the key, never a fallback to listing (0 `ListObjectsV2`).
- **`lith refresh <mountpoint>`**
  ([#170](https://github.com/scttfrdmn/lith/issues/170)): re-read `CURRENT` and, if
  a new version was published, **atomically hot-swap** the mount's index. Explicit —
  lith never polls. An open handle keeps the version it was opened against (versions
  are immutable, so its objects still exist), so an in-flight read stays consistent;
  only new lookups see the new version. Inodes are path-hashed, so a file in both
  versions keeps its inode across a swap — a cached `(dev, ino)` in `find`/`rsync`
  stays valid.
- **Pointer-based NFS gateway — `lith serve nfs s3://bucket/dataset@current`**
  ([#171](https://github.com/scttfrdmn/lith/issues/171)): serves a published dataset
  by name. The gateway does **not** hot-swap — a version change would `STALE` every
  NFS handle and NFSv3 is stateless — so a version-changing `refresh` is refused with
  an actionable message (restart the gateway to adopt); a same-version refresh is a
  no-op.
- **`pkg/lithindex`** ([#168](https://github.com/scttfrdmn/lith/issues/168)): a
  public, cross-repo package to build a lith index from a CargoShip 2.1 manifest —
  the producer-side library behind `cargoship publish` (cargoship#560). Its exported
  API is a compatibility surface, kept minimal.
- **`--log-level` on `mount` and `serve nfs`**
  ([#155](https://github.com/scttfrdmn/lith/issues/155)): `debug`/`info`/`warn`/`error`.

### Fixed

- **Byte-precise Parquet projection over-fetch**
  ([#125](https://github.com/scttfrdmn/lith/issues/125)): the demand path missed the
  `ProjectionCoalesceGap` cap that #153 wired to the plan path — on a fat NIC its
  concurrency-derived gap swept the non-projected columns between projected ones;
  now capped, and the fill metric splits `demand-batch` from `plan` so the two are no
  longer conflated ([#153](https://github.com/scttfrdmn/lith/issues/153)/[#156](https://github.com/scttfrdmn/lith/issues/156)).
  The projection learner now records columns by read-range overlap, not start offset,
  so `id`-at-offset-0 and coalesced reads are learned correctly
  ([#154](https://github.com/scttfrdmn/lith/issues/154)/[#157](https://github.com/scttfrdmn/lith/issues/157)).
  `--footer-tier2` stays **experimental, clustered-projections-only**: the measured
  floor for a spread projection is pyarrow's own bytes, and streaming wins on time.

### Docs

- **Published datasets** page
  ([#172](https://github.com/scttfrdmn/lith/issues/172)): what a published dataset
  is, publish/mount-by-name, pointer semantics, `refresh`, and the FUSE-mount-vs-NFS-
  gateway asymmetry; plus a "Copy or mount?" one-liner (*pack and publish once; mount
  it anywhere by name*).

### Known limits

`lith refresh` hot-swaps a single-client FUSE mount; the NFS gateway adopts a new
version by restart (NFSv3 statelessness makes a live swap unsafe). `--footer-tier2`
remains experimental (clustered projections only).

## [0.4.0] - 2026-09-13

**The NFS gateway: one node mounts, a cluster shares.** `lith serve nfs` exports a
lith mount over read-only NFSv3, so N compute nodes read a shared S3 dataset
through one node — fetched from S3 **once**, not once per node — with the second
job free. It replaces EFS/FSx-linked-to-S3 for read-only shared datasets: no
hydration, no filesystem minimum, no filesystem to create and delete.

### Added

- **`lith serve nfs s3://bucket[/prefix]`** ([#143](https://github.com/scttfrdmn/lith/issues/143)/[#144](https://github.com/scttfrdmn/lith/issues/144)):
  read-only NFSv3 gateway over `willscott/go-nfs`, serving the `Index` and
  `BlockStore` directly (no FUSE in the path). **Deterministic `(index-sha,
  inode)` file handles** — stable across a restart against the same index,
  `NFS3ERR_STALE` against a rebuilt one, no server-side handle table. Every
  mutating op is `NFS3ERR_ROFS`. **`READDIRPLUS`/`GETATTR`/`FSSTAT` served from
  the index** (0 S3). **Per-client fair-share** readahead window. `MOUNT` v3 on
  the same port (clients mount with an explicit port; no portmap). Metrics
  `lith_nfs_clients`, `lith_nfs_ops_total{op}`, `lith_nfs_read_bytes_total`.
- **`Index.ByInode(ino)`** ([#144](https://github.com/scttfrdmn/lith/issues/144)):
  reverse inode→path lookup for NFS handle resolution, through `View`; a sorted
  side array built lazily (~12 B/key).
- **`lith serve nfs --disk-cache/--disk-path`** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  a gateway sized to its working set serves a warm re-read and a restart from
  local disk, not S3.
- **`serve` flag parity with `mount`** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  the read-path/cache/S3 tuning flags (`--block-size`, `--max-range`,
  `--prefetch-budget`/`-concurrency`, `--inflight-bytes`, `--coalesce-gap`,
  `--s3-concurrency`, `--nic-gbps`, `--endpoint`, `--path-style`,
  `--auto-index-limit`) are on `serve`; FUSE-only/diagnostic flags are documented
  inapplicable. A test fails CI if any mount flag is silently missing.

### Changed

- **Gateway read path dispatches readahead concurrently** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  a cold single stream reads **1,463 MB/s** on loopback — *faster than the FUSE
  mount* (no FUSE hop); was 67 MB/s with the initial serial dispatch.

### Measured (N=8, CloudTrail-ledger-backed)

- **Shared dataset** (8 nodes read the same 8.88 GB bwa index): the gateway
  fetches it **once** — 1,139 GETs / 9.1 GB — where 8 independent mounts fetch it
  **8×** (8,561 GETs / 71.3 GB): **~1/8 the S3 traffic**. The second run issues
  **0 S3**. Cheaper than EFS (which needs ~4.5 min to hydrate 8.88 GB first) and
  FSx Lustre (1.2 TiB minimum).
- **Distinct data per node** (W3, 8 CRAMs): independent per-node `lith mount`
  wins (parallel across 8 NICs); the gateway is a one-NIC funnel. **The sizing
  rule: distinct data → per-node mounts; shared data → gateway.** See
  [Serving a cluster](https://github.com/scttfrdmn/lith/blob/main/docs/serving-a-cluster.md).

### Known limits

NFSv3 only (no NFSv4 state/delegations/ACLs); read-only; `AUTH_UNIX`/squash, no
Kerberos; per-**path** (not per-client) sequential state (go-nfs's stateless read
path); FSx Lustre client unavailable on arm64 Ubuntu (costed analytically).

## [0.3.2] - 2026-09-12

> Includes the [0.3.1] section below, which was never tagged — its CargoShip
> integration ships here (v0.3.1's tag was gated on an acknowledgment that the
> session never reached, and the next release picked up the slack). The compare
> link for this release points back to **v0.3.0** so the notes and `git log`
> agree.

CargoShip read efficiency and safety. The packed small-files win is now
**universal, not archive-specific**: a tree walk moves ~1× the archive's
compressed bytes regardless of the frame size (was up to ~3× at the 16 MiB
default). Recommended with cargoship **v0.24.5** and `--frame-size 4MiB`.

### Added

- **Decoded-frame cache** for CargoShip framed reads ([#137](https://github.com/scttfrdmn/lith/issues/137)).
  A fetched zstd frame is decoded **once** and served to every fill it covers — a
  byte-bounded LRU keyed by (chunk object, frame offset) with a **per-frame
  singleflight** so the concurrent prefetch fills of a large frame's many 1 MiB
  cache chunks don't each re-fetch it. A tree walk now fetches and decodes each
  frame exactly once at any frame size. New metric `lith_backing_frame_reuse_total`;
  `lith_backing_decompress_bytes_total` now counts each frame once. Budget
  defaults to 25% of the mem cache (min 64 MiB), allocated only for a CargoShip
  mount; a frame larger than the whole budget is served but not retained.
  Ledger-verified (CloudTrail S3 data events; lith counters match exactly), A1
  tree walk (1,985 files, ~13.1 MB compressed archive), cold:

  | frame size | before (main) | after (frame cache) |
  |---|---|---|
  | 16 MiB | 38.1 MB (2.9×), 23 GETs | **13.1 MB (1.0×), 11 GETs** |
  | 4 MiB | 19.7 MB (1.5×), 23 GETs | **13.1 MB (1.0×), 23 GETs** |

  A single 90 KB file's cold random read pulls only its one covering frame
  (1.1 MB at 16 MiB); a same-frame neighbour is 0 GETs. **Packed-Zarr (A2) drops
  2.66 GB → 0.76 GB (167 → 58 GETs)** as a side effect — Zarr chunks packed in
  key order share frames, so the frame cache serves neighbouring chunks without
  re-fetch; the CargoShip-plus-Zarr combination pays off twice. Frameless CRAM
  (A3) is unchanged (flagstat fetches the file size exactly, wall within 2% of
  native).

- **`lith mount --cargoship <manifest-url>`** ([#141](https://github.com/scttfrdmn/lith/issues/141)).
  Build the archive index in-process and mount it in one command. Mutually
  exclusive with `--index-file`; `lith mounts` shows `[cargoship:<key>]`.

### Fixed

- **`--cargoship` fails closed — it never falls back to listing the bucket**
  ([#141](https://github.com/scttfrdmn/lith/issues/141), P0). A manifest that
  cannot be resolved (missing, unparseable, unsupported version, encrypted) is a
  hard error naming the manifest key, for both `mount` and `index build` — with
  **zero `ListObjectsV2`** calls (regression-tested against a fake-S3 call log).
  Previously a failed `--cargoship` build left the index file absent, and a
  follow-up `lith mount --index-file` then **auto-listed the whole bucket**
  (session-36 benchmark artifact: 2,632 objects / 24 GB walked where one archive
  was intended). The catastrophic multi-chunk "thrash" once attributed to lith
  was this benchmark artifact, not a read-path bug; the real issue was the byte
  over-fetch fixed above.

### Docs

- CargoShip page: decoded-frame cache behaviour, one-command `mount --cargoship`,
  and "Choosing a frame size" (the tree walk is ~1× at every size; frame size now
  governs only random single-file over-fetch). Frame-size curve in
  [`bench/results/framesize-curve.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/framesize-curve.csv).

## [0.3.1] - 2026-09-12

CargoShip archive integration: mount a [CargoShip](https://github.com/scttfrdmn/cargoship)
2.1 archive as its **original file tree** and serve reads from the packed chunks.
Packing many small objects once and mounting them turns the many-small-objects
case (Zarr, sharded datasets) into large-object streaming — lith's best shape.

### ⚠️ Upgrade — rebuild your index

On-disk **index format is now v5** (adds an optional CargoShip backing section).
A v4 index is rejected with `index: unsupported format version 4 (this build
writes v5); rebuild the index with lith index build`. Rebuild any saved index.

### Added

- **`lith index build --cargoship s3://…/manifest.json[.gz]`** ([#94](https://github.com/scttfrdmn/lith/issues/94)).
  Builds an index whose namespace is the archive's original tree (no
  `ListObjectsV2`). The new `internal/cargoship` parses and hardens the 2.1
  manifest (lith#101: size cap, every offset bounds-checked, fuzzed), handling
  dedup and split files; incremental chains (`previous_manifest_id`) fold to a
  union-of-latest. `lith index inspect` prints archive/chunk/frame provenance.
- **CargoShip backing read path** ([#94](https://github.com/scttfrdmn/lith/issues/94)).
  A read of a virtual file maps to its packed chunk. **Framed** `.tar.zst` chunks:
  the covering zstd frame(s) — one coalesced range GET, each verified by its
  sha256-of-compressed checksum and decoded, cached as 1 MiB uncompressed chunks
  so files sharing a chunk share the cache. **Frameless** plain-`.tar` chunks
  (already-compressed/small content): a direct range GET at `archive_offset`, no
  decode (requires cargoship **v0.24.3**). Mixed archives route per chunk. New
  metrics `lith_backing_frames_fetched_total`, `lith_backing_decompress_bytes_total`,
  `lith_backing_checksum_fail_total`.

### Measured

A1 tree walk (1985 small files): **0.64 s / 12 GETs vs native 14.05 s / 1998
GETs** (22× faster). A2 one-month packed-Zarr query: **9.9 s ≤ native 10.7 s ≤
xarray+s3fs 14.3 s**. A3 tabix on a frameless VCF: within noise of native.

### Known limits

- A large file that CargoShip framed as one giant zstd frame is not randomly
  readable (a zstd frame isn't seekable); lith caps decodable frame size and
  errors clearly. Store large already-compressed files frameless, or cut
  sub-frames ([cargoship#502](https://github.com/scttfrdmn/cargoship/issues/502)).
- Encrypted (KMS-envelope) manifests are rejected; 2.0 archives are unsupported.

### Upstream (CargoShip) follow-ups filed
- [cargoship#492](https://github.com/scttfrdmn/cargoship/issues/492) `archive_offset`
  for every file — **fixed in v0.24.3** (enables the frameless read path).
- [cargoship#493](https://github.com/scttfrdmn/cargoship/issues/493) manifest
  records intermediate staging snapshots (lith keeps the complete entry).
- [cargoship#494](https://github.com/scttfrdmn/cargoship/issues/494) `chunk_id`
  identity is per-shard (lith keys chunks by `s3_key`).
- [cargoship#502](https://github.com/scttfrdmn/cargoship/issues/502) file-boundary
  framing makes a large file one giant non-seekable frame.

## [0.3.0] - 2026-09-10

The **format-aware plan** release. lith learns the on-disk shape of the formats it
serves — Zarr chunk grids, bgzf coordinate indexes, columnar footers — and reads
along the app's access pattern instead of blindly. The measurement rule holds: I/O
metrics (bytes, GETs) are primary; wall-clock is reported under I/O-bound configs.

### Added

- **Zarr grid-plane prefetch — Format tier 2 ([#70](https://github.com/scttfrdmn/lith/issues/70)/[#109](https://github.com/scttfrdmn/lith/issues/109)).**
  A chunked-store read now prefetches the whole grid-plane selection an access
  implies, not just the next chunks in key order — closing the 2-D boundary gap
  where uncovered chunks each paid a cold GET on the app's bounded pool. On the
  1.4 TB `chrtout.zarr` the I/O tail is closed; the remaining difference from an
  async in-place reader is the app's compute floor (a documented tie).
- **bgzf family — index-aware readahead, tiers 1+2 ([#107](https://github.com/scttfrdmn/lith/issues/107)).**
  A BAM/CRAM/VCF.gz/BCF data file with a coordinate-index sibling
  (`.bai`/`.tbi`/`.csi`/`.crai`) gets header prefetch + random protection at open
  (tier 1); at or below `--bgzf-whole-file-max` (512 MiB) it is fetched whole
  through the parts path when the index opens, and above it tier 2 prefetches only
  the index-resolved slice ranges. **2.8× fewer bytes on a point query**; wall is
  unchanged (samtools is decode-bound), so the win is byte-selectivity.
- **Footer family — Parquet/zip tier-1 + tier-2 ([#108](https://github.com/scttfrdmn/lith/issues/108)).**
  A columnar/archive container (Parquet/ORC/Arrow/zip) gets its footer (tail) +
  head prefetched at open (tier 1, always on). An experimental tier 2
  (`--footer-tier2`, **off by default** — see Changed) fetches the index-resolved
  projection/entries byte-precise.
- **Sparse chunk fills + coalescing substrate ([#118](https://github.com/scttfrdmn/lith/issues/118)/[#124](https://github.com/scttfrdmn/lith/issues/124)/[#31](https://github.com/scttfrdmn/lith/issues/31)).**
  The 1 MiB cache chunk gains a 64 KiB-granularity filled-extent bitmap: a byte-
  exact plan range and a non-sequential point read fetch only the extents they
  cover, not the whole chunk; sequential reads still fill whole chunks. Fill
  batches coalesce extents across chunks into range GETs, merging gaps up to
  `--coalesce-gap` (default derived from NIC × first-byte latency ÷ usable
  concurrency, clamped [256 KiB, 64 MiB]), and dispatch their runs concurrently.
  The disk tier persists the bitmap. New metrics `lith_fill_partial_total`,
  `lith_fill_runs_total`, `lith_fill_bytes_total{kind=plan|demand|whole|gap}`,
  `lith_fill_gap_bytes_total`, `lith_fill_batch_size`, `lith_fill_inflight`,
  `lith_fill_inflight_peak`.
- **Read-path metrics ([#65](https://github.com/scttfrdmn/lith/issues/65)).** A
  read-size histogram (`lith_read_size_bytes`) and a distinct-object-bytes-read
  gauge (`lith_distinct_bytes_read`) from per-object 64 KiB touched-extent bitmaps.
- **`lith index build --keys <file>` / `--keys-from-manifest <url-or-key>`
  ([M6](https://github.com/scttfrdmn/lith/issues/106)).** Build an index from an
  explicit key list, for buckets that are GET-public but deny `ListObjectsV2`
  (Common Crawl `cc-index`, `nyc-tlc`). One key per line (`#` comments; optional
  `\t<size>\t<mtime>` columns); keys lacking size/mtime are `HeadObject`-ed with
  bounded concurrency (`--s3-concurrency`, `--no-sign-request` honored). A 403/404
  is counted and listed, failing the build unless `--keys-allow-missing`. A
  manifest is fetched (`.gz` transparently) then parsed the same way. The index
  records its build source and the key-file sha256; `lith index inspect` prints them.

### Changed

- **`--footer-tier2` defaults to off (experimental).** Byte-precise Parquet
  projection fetch is measured slower than default whole-file streaming on every
  tested instance class, in-region: a whole-file stream is a handful of coalesced
  GETs at line rate, while byte-precise fetch pays a round-trip per column chunk
  through a filesystem that sees reads one at a time. Criterion 3 is **met by
  streaming**; tier 2 ships as a correct, tested, opt-in substrate, and the
  precision work moves to [#125](https://github.com/scttfrdmn/lith/issues/125)
  (v0.4). With tier 2 off a footer handle streams exactly like a plain one (tier 1
  footer+head prefetch only).
- **`lith_distinct_bytes_read` now counts filled 64 KiB extents, not 1 MiB
  chunks** (it was chunk-rounded). Values are finer-grained (and smaller) than
  before for sub-chunk access ([#118](https://github.com/scttfrdmn/lith/issues/118)).
- On-disk index format bumped to **v4** (build-provenance trailer: source +
  key-file sha256); the disk block-cache format bumped to **v2** (per-chunk extent
  bitmap). Both are private — older files are ignored/rejected and rebuilt.

## [0.2.2] - 2026-09-10

Security release: two internal audit passes, fully remediated. Defensive hardening
only — no on-disk format change, no new mechanisms.

### Security

Remediation of an internal security audit (defensive hardening; no format change).

- **`lith umount` no longer signals an unverified PID.** A mount record's PID is
  signalled only if it is genuinely among the processes holding the mount open
  (`fuser -m`), and mount records are read only from a runtime dir the current
  user owns; otherwise umount falls through to `fusermount3 -u`. This closes a
  local-multi-user vector where a planted record in a world-writable fallback dir
  could induce `lith umount` (esp. as root) to SIGTERM an arbitrary PID.
- **Runtime records and the disk cache are created private and ownership-checked.**
  The per-user runtime dir and the disk-cache dir are created `0700` and refused
  if they are a symlink or not owned by the current user; mount records are
  written `0600`; the daemon log is per-uid, `0600`, and opened `O_NOFOLLOW`.
- **`--endpoint` may not receive signed requests over plaintext.** A non-`https`
  endpoint is rejected unless `--no-sign-request` is set, so SigV4-signed requests
  (and the STS session token) are never sent in cleartext to an arbitrary host.
- **Ranged GETs are validated.** A response that ignores the requested byte range
  (returns the whole object) is rejected via its `Content-Range`, so offset-shifted
  data can never be served as the requested range.
- **`lith mounts`/`umount --all` match the exact `fuse.lith` fstype**, no longer a
  substring, so unrelated FUSE mounts (`fuse.monolith`, …) are never touched.
- **System helpers are resolved from a fixed trusted path** (`/usr/bin`, `/bin`,
  `/usr/sbin`, `/sbin`) rather than `$PATH`.
- **Keys containing C0 control bytes are rejected** at index build.
- **The index is memory-mapped `MAP_PRIVATE`**, immune to post-validation mutation
  of the backing file.
- **The HTTPS-endpoint guard now covers the environment**, not just `--endpoint`:
  a non-`https` endpoint resolved from `AWS_ENDPOINT_URL`/`AWS_ENDPOINT_URL_S3`/
  shared-config `endpoint_url` is also rejected when signing, so credentials can't
  reach a cleartext endpoint via the standard SDK config path.
- **`--metrics` no longer serves pprof.** `net/http/pprof` (which exposes the process
  argv and an on-demand CPU/goroutine profiling DoS) moved to a separate opt-in
  `--pprof <addr>` flag (off by default; bind to localhost). `--metrics` serves only
  `/metrics`, and block/mutex profiling is enabled only when `--pprof` is set.
- **The NIC-bandwidth cache (`nic.json`) is written safely.** It is now a per-uid
  file created with `O_EXCL|O_NOFOLLOW` (`0600`) and read only when it is a regular
  file owned by the current user — closing a symlink/pre-created-file overwrite in
  the predictable `$TMPDIR` path and a cross-user sizing-poisoning read.
- **`ethtool` and the bench `ss` helper are resolved from the fixed trusted path**
  (as `fusermount3`/`fuser` already were).
- **Supply chain:** CI now runs `govulncheck`; all GitHub Actions are pinned to
  commit SHAs and `goreleaser-action` to a fixed version; `nic*.json` is gitignored.

### Fixed

- **Truncated S3 responses are no longer cached or served as valid zeros.** A short
  chunk read (dropped/partial transfer) now fails the fill instead of completing a
  zero-padded buffer that was persisted to the memory and disk tiers and served as
  authoritative on every later read.
- **A crafted directory offset can no longer crash the mount.** `Readdir` clamps an
  out-of-range cursor and the FUSE readdirplus path no longer underflows, closing a
  local denial-of-service (out-of-bounds panic). FUSE query handlers also recover a
  panic into `EIO` rather than tearing down the mount for all users.
- **The S3 Inventory parser is bounded.** Manifest, per-file decompression, and total
  entry count are capped, so a hostile or malformed inventory (gzip bomb, giant
  manifest) errors instead of exhausting memory. Inventory keys are unescaped with
  `PathUnescape` (a literal `+` is preserved) and a negative object size is rejected.
- **`--timeline-csv` output is injection-safe.** The diagnostic CSV is now written with
  `encoding/csv` and cells beginning with `= + - @` (or tab/CR) are prefixed with `'`,
  so an S3 key name cannot corrupt the CSV structure or become a live spreadsheet formula.
- **`parseSize` rejects `Inf`/`NaN`/negative/overflowing values** instead of silently
  yielding a garbage size.
- **The daemon log is only appended to a file the current user owns** (an attacker
  pre-creating a regular file at the predictable path is now refused), and the mount-record
  write closes a `/tmp`-fallback symlink race (`Mkdir`+re-check, `O_NOFOLLOW`).

## [0.2.1] - 2026-09-10

Hardening and docs currency from an external review of v0.2.0. No new mechanisms.

### Fixed

- **The release workflow no longer publishes on a red commit** (#98): the tag
  build now depends on a `gate` job that reruns the full CI suite (vet, lint,
  `go test -race`) for the tagged SHA; goreleaser runs only if it passes. A tag
  push does not itself trigger CI, so the gate lives in the release workflow.
- **Flaky prefetch test** (#98): `TestPrefetchAheadCoversDemand` and
  `TestMidFillUnblock` synchronized on the fake S3 signalling a GET in flight
  (new `OnGetStart` hook) instead of sleeping; `-race -count=20` clean.
- **`View.TotalSize` data race** (#102): the per-mount-root size is summed once
  at `Root()` and read as an immutable field, so concurrent `StatFs` on a
  prefix view is race-free (was a lazily-set field with no lock).
- **The index deserializer now validates every length and arena offset against
  the image before slicing** (#101): a malformed image returns `ErrCorruptIndex`
  naming the byte offset instead of panicking. A fuzz test over the parser
  (truncated + bit-flipped corpus) runs as a regression corpus in CI.
- **Install one-liner** (#99): releases now also publish stable-name raw
  binaries (`lith_linux_amd64`, `lith_linux_arm64`) alongside the versioned
  `.tar.gz` archives, so `curl -L …/releases/latest/download/lith_linux_amd64`
  works across releases.

### Changed

- **Index format → v3** (#101): the arena offset arrays (`offs`/`dirOffs`) widen
  from `uint32` to `uint64`, lifting the ~4 GiB pathname-arena cap (measured
  cost: +4.0 bytes/key, ~4.6%; the zero-copy mmap path is unchanged). v2 index
  files are rejected on load with the rebuild message, as v1 → v2.
- **Docs currency** (#100): Zarr numbers reconciled to the shipped tier-1
  figures (~35 s cold vs ~25 s in-place s3fs, same box; grid-aware prefetch is
  shipped, not future work), README command table and feature list updated to
  v0.2, a "Why 'lith'" note added to the README and docs, and every page walked
  against `--help` and the CHANGELOG (`mkdocs build --strict`).

### Security

- **SECURITY.md** (#100): dropped the `security@example.com` placeholder;
  vulnerability reports go through GitHub private vulnerability reporting.

## [0.2.0] - 2026-09-10

### Added

- **Mount root at a prefix** (#90): `lith mount s3://bucket/some/prefix /mnt`
  roots the filesystem at `some/prefix/`, showing only what lives under it with
  the prefix stripped from every path. An index built at that prefix — or any
  parent of it, including a whole-bucket index — serves the mount with no
  rebuild, so **one index can back many prefix mounts concurrently**. A prefix
  the index cannot cover, or one with no keys under it, is rejected rather than
  mounted empty. `lith index inspect` now prints the index `root:`.
- **`lith umount` and `lith mounts`** (#91): `lith umount <mountpoint>` signals
  the mount process and waits for it to leave the mount table, falling back to
  `fusermount3 -u` (and `-uz` lazy only with `--force`); a busy mount is
  reported with the holding pids and refused without `--force`. `--all`
  unmounts every lith mount for the user. `lith mounts` lists live mounts
  (cross-checked against `/proc/mounts`) and stale records, with `--prune`.
- **Sibling readahead for chunked stores** (#63): when successive opens in a
  directory are close in index order (a detected directory walk), lith
  prefetches the next N siblings whole — turning many-small-object access (Zarr
  chunks, WebDataset shards, per-chromosome BAM) into large-object streaming,
  lith's best shape. New `--sibling-window` (default 4, the max index-position
  gap that still counts as walking) and `--sibling-readahead` (default 16;
  0 disables).
- **Parallel parts for mid-size files** (#69): a file at or below `--parts-max`
  (default 64 MiB) is fetched on first read as concurrent block-sized range GETs
  instead of one serial stream, so a mid-size file reaches full bandwidth
  without waiting on a single sequential fill.
- **Zarr grid-aware readahead — the first format-aware access plan** (#70,
  tier 1): on a `.zarray`/`.zmetadata`-shaped store, sibling readahead follows
  the chunk grid along the walked axis instead of flat key order, so
  grid-boundary jumps no longer reset the walk. On the NWM `chrtout.zarr` year
  read this halved the uncovered misses (114 → 64) with 100%-used prefetch.
- `lith mount --timeline-csv <path>`: an **opt-in** per-chunk diagnostic
  timeline (per demanded chunk: join-wait, in-flight fill depth, and the lag
  from a covering prefetch's dispatch to the app's open) for prefetch
  investigations. No effect on the read path when unset (#95, #70).
- `--nic-gbps` on `lith mount` overrides the detected NIC bandwidth (which sizes
  `--inflight-bytes` and the readahead window) (#79).
- **Documentation site** (mkdocs-material, #80): Start here, Copy or mount?,
  Sizing, Deadline, Knobs, and What lith is not.

### Changed

- **The in-flight budget is now sized from NIC baseline bandwidth** (#79):
  NIC detection is ethtool → EC2 `DescribeInstanceTypes` baseline → a fixed
  fallback (was ethtool → 512 MiB), and both the `--inflight-bytes` default and
  the readahead window derive from the baseline. This fixes burst-credit
  instance classes (e.g. `c8g`) the old bandwidth table missed and silently
  defaulted to 512 MiB in-flight. The NIC result is cached in `nic.json` next to
  the index so repeat mounts and boxes without `ec2:DescribeInstanceTypes` still
  get a real answer.
- **Unified prefetch limits** (#64): per-handle readahead, sibling readahead,
  and the parts path now draw on one shared prefetch budget and query the index
  for neighborhoods through a single policy, so aggregate readahead stays
  bounded by the memory tier across all three paths (extends #55).

## [0.1.0] - 2026-09-08

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

- A read contained within one chunk returns a sub-slice of the (immutable,
  cached) chunk buffer directly to the kernel — no allocation and no copy on a
  cache hit — and go-fuse splices the reply zero-copy. Boundary-straddling
  reads still assemble and are counted in `lith_read_straddle_total` (≈0 for
  sequential reads, which fall entirely within a chunk).
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

- **Aggregate readahead is now bounded by the memory tier**, so concurrent
  readers no longer thrash a small cache (#55). Each open handle's readahead
  window is capped at `prefetch-budget / open-handles` (floor 2, capped by
  `--max-readahead`), and memory-tier eviction prefers already-read chunks over
  prefetched-but-unread ones. On a 16 GiB `c8g.2xlarge`, the cold 8-reader
  aggregate went from 180 MB/s (prefetch thrash: prefetched chunks evicted
  before use and re-fetched) to **1550 MB/s, 97% of mountpoint-s3**; a 32 GiB
  `c8gd.4xlarge` went 1095 → 1688 (65% → 103%). New `--prefetch-budget`
  (default 50% of `--mem-cache`) and metric `lith_prefetch_evicted_unread_total`
  (the thrash signal, ~0 after the fix).
- **Single-reader cold sequential now fills a fat NIC.** `--max-readahead`
  defaults to 1.5× the bandwidth-delay product (`inflight-bytes / block`) instead
  of a fixed 64 blocks, so one reader holds enough in flight to saturate the
  link. On a 30 Gbps `c8gd.16xlarge`, cold single-reader went 1737 → ~2850 MB/s
  (67% → ~109% of mountpoint-s3) (#56). The **1.5× multiplier is empirical, not
  theory** — derived from measurement on that box: a raw-BDP window (~89 blocks)
  reached only 88% of mountpoint-s3 because completed chunks sit cached-unread
  ahead of the cursor, so the bytes actually in flight are less than the window;
  1.5× closes the gap. Re-derive if the workload or instance profile changes.

- The per-handle prefetcher is now **tolerant of out-of-order reads**. The kernel
  issues a single file handle's readahead concurrently, so reads can reach the
  FUSE layer out of order even for a strictly sequential file; the old detector
  treated any non-unit delta as a pattern break and collapsed the readahead
  window to zero, then re-ramped from 2. Under 8 concurrent readers this left the
  last, largest, slowest-draining file with an oscillating shallow window — the
  bimodal ~180 MB/s cold tail (#49). The detector now treats a read within the
  reorder band `[cursor-window, frontier+window]` as sequential progress; a
  genuine out-of-band seek **halves** the window (floor 2) and re-anchors rather
  than resetting to zero, and only two seeks with no progress between them fall
  to random. New metrics `lith_prefetch_window_halved_total` and
  `lith_prefetch_reset_random_total`. See #49, #40.
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

- `--prefetch-concurrency` on `lith mount` and `lith bench` caps concurrent
  prefetch fills; it defaults to `--s3-concurrency` (previously the prefetch
  sub-limit was hardwired to half of `--s3-concurrency`, capping concurrent
  fills at 64 at the default and holding the 8-reader aggregate near the
  ~64-connection ceiling). Isolating this one change lifted the cold 8-reader
  aggregate on a c8gd.16xlarge from ~2300 to ~3000 MB/s. See #40.
- `--max-range` on `lith bench` (already present on `lith mount`): caps the
  coalesced range-GET size; the bench previously coalesced at most one block.
- `cmd/lith-s3bench`: a standalone diagnostic that isolates the S3
  client/transport (N workers, back-to-back ranged GETs, no cache/prefetch/
  FUSE). It established that the Go client sustains ~3.5 GB/s at 128 concurrent
  GETs on a 30 Gbps box — above mountpoint-s3 — so the multi-reader ceiling is
  in lith, not the transport. Tooling; not part of the shipped filesystem.
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

[Unreleased]: https://github.com/scttfrdmn/lith/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/scttfrdmn/lith/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/scttfrdmn/lith/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/scttfrdmn/lith/compare/v1.1.3...v1.2.0
[1.1.3]: https://github.com/scttfrdmn/lith/compare/v1.1.2...v1.1.3
[1.1.2]: https://github.com/scttfrdmn/lith/compare/v1.1.1...v1.1.2
[1.1.1]: https://github.com/scttfrdmn/lith/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/scttfrdmn/lith/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/scttfrdmn/lith/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/scttfrdmn/lith/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/scttfrdmn/lith/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/scttfrdmn/lith/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/scttfrdmn/lith/compare/v0.3.0...v0.3.2
[0.3.1]: https://github.com/scttfrdmn/lith/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/scttfrdmn/lith/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/scttfrdmn/lith/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/scttfrdmn/lith/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/scttfrdmn/lith/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scttfrdmn/lith/releases/tag/v0.1.0
