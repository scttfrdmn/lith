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

## What is not settled

Whether a **cumulative** byte budget — one that permits the establishment burst while still
capping total over-fetch across the run — keeps the byte saving. The burst is 1.78 GB and the
low-coverage arm's entire distinct read was 22.5 MB, so it may not. That is a design question
to score offline against the banked traces before it is worth anyone's endpoint.
