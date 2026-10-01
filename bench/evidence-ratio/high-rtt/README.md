# `--readahead-evidence-ratio` at high RTT: the cost is a capped dispatch burst

The [in-region gate](../README.md) showed the flag is free on a pure sequential stream —
byte-identical, no extra requests, no wall-clock signal. At **58.6 ms RTT** it is not free: it
costs a clean **2.56×**. This is what that is.

Measurements are the reporting workload's (an external GCHP deployment on `lith#256`), from a
head node in us-east-1 against objects copied to us-west-2. The **analysis** is ours, computed
offline from their published `--pf-trace` CSVs; `analyse.sh` reproduces it from the archive.

## The effect, on an object larger than the prefetch window

`GEOSFP.20190701.A3dyn.025x03125.nc`, **3,776,834,855 B = 450.4 blocks**, so the 223-block
window is 49.5% of the object and binding for the whole read. `dd`, fresh cold mount per cell,
n=8, ratios interleaved within rep.

| `k` | median wall | spread |
|---|---|---|
| off | **8.51 s** | 7.19 … 12.49 |
| 4 | **21.74 s** | 17.16 … 29.11 |

`min(k=4) = 17.16 > max(off) = 12.49` — **zero overlap**, 64/64 pairs, exact rank-sum
p = 1.6×10⁻⁴, median ratio **2.56×**. Every cell moved `lith_s3_bytes_total = 3,776,834,855`
in **465 GETs**. In-region on the same object: **+0.215 s, +7.5%** (n=8, U = 52/64, p ≈ 0.03),
so the effect is RTT-scaled and visible without a distant endpoint — 26.7× the RTT buys 62×
the penalty.

**Earlier reports of a *bimodal* 4 s / 20 s split were an artefact of a smaller test object.**
At 851 MB = 101.5 blocks, the window (223 off, 163 mean on) is 1.6–2.2× the *whole object*, so
readahead extent was never the binding constraint and the arms could not be distinguished by
any window-mediated mechanism.

## The cause: one dispatch event

`advance(cursor+1+window)` emits `[frontier, target)` in a single call, so **the window value
at establishment is the batch size**. [#229](https://github.com/scttfrdmn/lith/issues/229)
makes that batch the full window on purpose, its comment naming the alternative: *"many small
GETs, an underfed NIC on the cold read — the #56 concern"*. The evidence gate caps it
(`p.window = p.windowCap()` in the establish branch), which is that regression, reintroduced.

Batch sizes from the traces — same 450 batches and 672 blocks in every arm, which is why bytes
and requests are identical:

| `k` | batches | blocks | **max batch** | p99 batch |
|---|---|---|---|---|
| off | 450 | 672 | **223** | **1** |
| 40 | 450 | 672 | 41 | 24 |
| 4 | 450 | 672 | 5 | 5 |
| 1 | 450 | 672 | 2 | 2 |

With the gate off, one read dispatches 223 blocks and every later dispatch is **one** block.
That single burst is the entire throughput advantage.

## It resolves to concurrency, which is why raising `k` cannot rescue it

| `k` | burst | throughput | concurrent streams' worth | median wall |
|---|---|---|---|---|
| off | 223 | 443.8 MB/s | **3.10** | 8.51 s |
| 40 | 41 | 283.8 MB/s | 1.98 | 13.31 s |
| 4 | 5 | 173.7 MB/s | 1.21 | 21.74 s |
| 1 | 2 | 160.4 MB/s | **1.12** | 23.55 s |

One stream is 8 MiB / 58.6 ms = 143 MB/s; a fully serial read is 465 × 58.6 ms = **27.2 s**.
`k=1` at 1.12 streams is essentially serial, and 23.55 s sits just under that floor.

Concurrency cannot fall below one stream, so the penalty **saturates**: a 40× range in how long
the cap binds (50 MB at `k=40` to 1871 MB at `k=1`) buys only a **3.1×** range in excess
(+4.80 s to +15.04 s). A naive pipelining account predicts excess ∝ 1/k, i.e. 1 : 0.25 : 0.025
for k = 1 : 4 : 40; measured is 1 : 0.88 : 0.32 — far flatter.

**The gate bounds committed readahead bytes, and on a long pipe committed bytes are what buy
concurrency. The byte saving and the burst are the same quantity, so no ratio keeps both.**

## Hypotheses tested and refuted, in order

| # | hypothesis | refuted by |
|---|---|---|
| 1 | low throughput slows evidence accrual, so the window grows slower | code: `windowCap()` is `floor(consumed × k / blockSize)` and `consumed` advances per read before any gate, so the schedule is bit-identical at any RTT |
| 2 | the window floor is below one round trip, so the reader outruns prefetch | measurement: lifting every sub-round-trip window left the rate unchanged, 4/8 → 4/8 ([#291](https://github.com/scttfrdmn/lith/pull/291), reverted in [#293](https://github.com/scttfrdmn/lith/pull/293)) |
| 3 | demand reads join in-flight prefetches and wait on *their* round trip | offline `-lead`: minimum lead is 127 reads in **both** arms — 8.1 round trips at the slow rate — and not one read is under it |
| 4 | the cap defers dispatch into one late giant burst | offline batch sizes: it is the **fast** arm that bursts (223 blocks); the slow arms never exceed 41 |

(2) and (4) were ours and both were wrong. (3) was ours and was killed offline before it cost
anything. Nothing here needed a fifth cross-region gate; (3) and (4) came from traces already
published.

## Reproducing

```
curl -L -o bigobj.tgz https://github.com/scttfrdmn/aws-gchp/raw/lith/input-layer/data/lith-gates/xregion-bigobj-traces.tgz
# sha256 55764044a1be804ff3156869957b73b989ab09b4325919c87fd1e27b769f1004
mkdir t && tar xzf bigobj.tgz -C t
go build -o /usr/local/bin/lith-pfreplay ./cmd/lith-pfreplay
bench/evidence-ratio/high-rtt/analyse.sh t
```

`xrbig.log` (cross-region n=8), `irbig.log` (in-region n=8) and `xrlad.log` (the k ladder, n=3)
are the raw gate output. The ladder's predictions were pre-registered in the reporting
workload's repo before it ran.

## A cumulative byte budget cannot fix it — scored, not argued

The obvious escape is to bound **cumulative** prefetched bytes over the run rather than the
instantaneous window, permitting the establishment burst (concurrency) while still capping total
over-fetch (the byte saving). Scored against the banked second-workload traces, it is
irreconcilable:

| arm | object | distinct read | coverage | full burst, EOF-clamped |
|---|---|---|---|---|
| met/var1 | 1217.8 MB | 24.1 | 0.020 | **1217.8** |
| met/sub | 1217.8 | 27.3 | 0.022 | **1217.8** |
| hco/var1 | 851.4 | 87.0 | 0.102 | **851.4** |
| hco/sub | 851.4 | 29.4 | 0.034 | **851.4** |

223 blocks is 1.78 GB, larger than every object in the set, so the burst clamps to the whole
object. **Any policy that permits a full establishment burst fetches the entire object**, and
the saving on every low-coverage arm is exactly **zero** — met/var1's 95.1% becomes 0%.

Making the budget cumulative does not help, because of ordering: at establishment `consumed` is
one block crossing (~8 MiB), so a `k × consumed` budget at k=4 is 32 MiB and refuses the burst
exactly as the instantaneous bound does. Permitting it on credit against future consumption
fetches everything *before* the evidence that would have denied it exists. The burst completes
first either way.

## The remaining candidate, and the half that is not offline-answerable

The burst's **value** is request count in flight; its **cost** is count × unit. lith's prefetch
unit is a block (8 MiB), so the same 223 requests at *chunk* granularity (1 MiB) would commit
223 MB rather than 1.78 GB — same concurrency, one-eighth the bytes:

| arm | gate off | gate k=4 | chunk-granular burst |
|---|---|---|---|
| met/var1 | 1217.8 MB | 59.8 | **233.8** (−80.8% vs off, +291% vs k=4) |
| hco/var1 | 851.4 | 371.8 | **233.8** (−72.5% vs off, **−37% vs k=4**) |
| met/whole | 1217.8 | 1217.8 | 1218.4 (unchanged) |

A different point on the frontier rather than a free lunch — it gives back most of the win on
one arm while beating the gate on the other.

**Resolved by intervention, and it kills the unit story.** `--s3-concurrency` is a flag, so the
suspected fourth ceiling could be perturbed directly rather than fitted. In-region, same object,
gate off, realized `peak_window` verified at 223 in every A cell and 1024 in every B cell, so
concurrency does not move the depth ceiling and the only variable is slots:

| `--s3-concurrency` | A = 8MiB/223 | B = 1MiB/1024 | B/A |
|---|---|---|---|
| 32 | 1437 MB/s | 922 MB/s | **0.642** — B is 1.56× *slower*, 9/9 separation |
| 128 | 1377 | **1630** | 1.183 — B faster, 9/9 separation |
| 512 | 1268 | 1253 | 0.988 — null |

**The sign inverts at 32 slots.** 32 × 1 MiB is 34 MB in flight against 32 × 8 MiB at 268 MB, and
the big unit wins when slots are scarce. So **the 1 MiB advantage is not a property of the unit —
it is a property of bytes-per-slot, and in-region it exists only in a band around the shipping
default of 128.**

A prediction of ours failed in the same run: we expected B/A to keep rising with concurrency,
since depth 1024 can backlog 512 slots and depth 223 cannot. It does not (0.642 → 1.183 →
0.988), and 128 → 512 makes *both* configurations slower. 128 is an optimum to sit near, not a
ceiling to raise.

Neither axis orders the six cells on its own — the fastest (1630 MB/s) is at 134 MB in flight,
while 268 MB gives 1437 and 537 MB gives 1253. There is an interior optimum in two variables and
six points cannot locate it; we are not fitting one, for the reason the rest of this file
documents.

### Consequence: shrinking the default `--block-size` is parked

The 1.58× cross-region result is real and reproduces, but its benefit is **contingent on
`--s3-concurrency` staying at its default**, and at 32 slots the same change is a **1.56×
regression**. Lower concurrency is exactly what a small instance, a shared endpoint, or
politeness to S3 would choose. A default whose benefit depends on another default holding still,
and which harms anyone who moved it, is not a default change — so this is parked rather than
pursued, upstream of the coalescing / `--parts-max` / bgzf / footer questions it would also have
had to answer.

Noted but **not acted on**: at 8 MiB in-region the shipping default is flat-to-declining in
concurrency (1437 / 1377 / 1268 MB/s, best at the lowest setting tested), which would put
`--s3-concurrency 128` ~4% behind 32 on this one object. That contradicts
[#40](https://github.com/scttfrdmn/lith/issues/40), where 128 won a tuning grid across cold
sequential and stride. One object at n=3 with soft magnitudes does not overturn a grid; it is a
reason to re-run the grid, not to change the number.

**The original question — whether unit-shrinking is a design worth building — is answered no.**
