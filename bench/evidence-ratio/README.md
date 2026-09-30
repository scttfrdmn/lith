# `--readahead-evidence-ratio`: the request-side cost on a whole-file read

The flag ([#256](https://github.com/scttfrdmn/lith/issues/256)) bounds a committed readahead
window to a multiple of the bytes a handle has actually read. It is aimed at the **window
over-commitment** waste channel, where a single-handle sequential reader that wants a small
slice of a large object earns the full NIC-derived window and fetches the whole thing.

On the channel it addresses, the reporting workload measured **−95.1% / −56.3% bytes with
byte follow-through *rising*** 0.005 → 0.135 and 0.083 → 0.190. The open question before the
flag could graduate from experimental was the **cost**, which lands on the opposite case: a
reader that wants every byte pays for the window ramp. They measured **+22.3% GETs for +0.07%
bytes** on a whole-file read and correctly declined to claim a wall-clock result at n=1.

This gate prices that cost on a pure sequential stream.

## Method

`c7g.4xlarge`, us-east-1, 16 vCPU / 30 GB, ~20 min, **$0.19** at $0.58/hr on-demand.
In-region reads of `s3://1000genomes/release/20130502/` — free egress, negligible request cost.

- Reader: **`cat` to `/dev/null`** — one contiguous sequential stream.
- Objects: `chr20` VCF (326 MiB) and `chr1` VCF (1161 MiB — near-identical in size to the
  1.22 GB MERRA-2 object the reporting workload used).
- `--nic-gbps` ∈ {50, 7.5, 3}, which derives `max_readahead` ∈ {223, 33, 13} blocks. 223 is
  the regime the reporting workload measured at.
- `--readahead-evidence-ratio` ∈ {0 (off), 4}.
- 5 reps per arm, **rep 1 discarded as warm-up** (v1 showed a monotone within-arm trend).
- Both arms are lith, so lith's own counters are the right instrument; the CloudTrail ledger
  exists for comparing lith against a *different* reader, not against itself.

**On `--nic-gbps 50` for a box whose real baseline is 7.5.** This over-sizes the window
relative to the pipe ([#239](https://github.com/scttfrdmn/lith/issues/239)), which would be
illegitimate for a partial-read byte measurement. It is sound here: a whole-file reader wants
every byte regardless of window size, so the bytes fetched are identical by construction and
the only thing the declared NIC changes is the ramp length and request pattern — which is what
is being measured. It is also the only way to reach `max_readahead=223` on a cheap box.

## Result: on a pure stream the flag is free

| object | `max_readahead` | ratio | bytes | GETs | `issued` | wall mean |
|---|---|---|---|---|---|---|
| chr20 326 MiB | 223 | 0 | 341,680,844 | 42 | 326 | 0.44 s |
| chr20 326 MiB | 223 | **4** | **341,680,844** | **41** | 326 | 0.37 s |
| chr1 1161 MiB | 223 | 0 | 1,217,111,389 | 154 | 1153 | 1.20 s |
| chr1 1161 MiB | 223 | **4** | **1,217,111,389** | **154** | 1153 | 1.30 s |
| chr20 | 33 | 0 | 341,680,844 | 42 | 326 | 0.35 s |
| chr20 | 33 | **4** | **341,680,844** | **42** | 326 | 0.38 s |
| chr1 | 33 | 0 | 1,217,111,389 | 154 | 1153 | 1.09 s |
| chr1 | 33 | **4** | **1,217,111,389** | **154** | 1153 | 1.28 s |
| chr20 | 13 | 0 | 341,680,844 | 42 | 326 | 0.55 s |
| chr20 | 13 | **4** | **341,680,844** | **42** | 326 | (see below) |
| chr1 | 13 | 0 | 1,217,111,389 | 154 | 1153 | 2.46 s |
| chr1 | 13 | **4** | **1,217,111,389** | **154** | 1153 | 2.37 s |

- **Bytes are byte-identical in all six comparisons** — not +0.07%, exactly zero, at every
  window size. Same for `lith_prefetch_issued_total`.
- **Requests do not increase**: 42 → 41 and 154 → 154. The +22.3% did **not** reproduce.
- **No wall-clock signal.** One arm showed a 2.78 s mean from a single 9.32 s rep; re-run at
  n=8 it resolves to noise, with ratio 4 marginally *faster* (median **0.52 s** vs 0.56 s).

## What the discrepancy means

Both measurements are real and the reconciliation is the **reader**, not the flag. The
reporting workload's whole-file arm is `netCDF4`/`h5py` walking an object **variable by
variable** — many discontiguous reads, so accrued evidence lags the window repeatedly and the
ramp is paid more than once. `cat` is one contiguous stream: evidence accumulates as fast as
the window grows, so the bound never binds.

So the ramp cost is a property of *how* a whole-file reader asks, not an intrinsic cost of the
flag. Stated as the three cases:

| reader | bytes | requests |
|---|---|---|
| pure stream (`cat`, `cp`) | **0%** | **0%** |
| structured whole-file (`h5py` all variables) | +0.07% | +22.3% |
| low-coverage slice reader | **−56% to −95%** | fewer |

## Reproducing

`run.sh` on any box with lith on `PATH` as `/tmp/lith`; `gate2.log` and `re.log` are the raw
output of the run above. `--pf-trace` CSVs for every arm were captured and are archived with
the run.

## Status

This does not by itself graduate the flag. It removes the cost objection for stream readers
and narrows the remaining one to structured whole-file readers, which the reporting workload
has already measured. Neither side has a convincing wall-clock result — theirs is n=1, this
one is inside the noise floor — but bytes and requests are counters and do not need
repetition.
