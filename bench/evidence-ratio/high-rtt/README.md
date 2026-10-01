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

**The throughput half cannot be answered offline, and the arithmetic that would do it has already
failed once.** 443.8 MB/s at 58.6 ms needs only 26 MB in flight, so 223 MB looks ample — but the
`k=4` arm had 5-block batches (42 MB, implying 717 MB/s by the same reasoning) and measured
**173 MB/s**. In-flight bytes are not the limiter; request count and scheduling are. Settling it
needs one measured arm: cross-region on the 3.78 GB object with `--block-size 1MiB` and the gate
**off**, which isolates request-size from everything else and requires no new code. If
throughput holds near 443.8 MB/s, unit-shrinking is a real design; if it collapses toward 173,
concurrency needs depth as well as count, the frontier has no better point, and the flag is
in-region-only permanently.
