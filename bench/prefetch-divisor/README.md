# Does the prefetch-budget divisor earn its cost? (#301 Phase 1)

`perHandleWindow()` rations windowed readahead as `clamp(prefetchBudget/blockSize/openHandles,
2, maxReadahead)`. Measured on a real mount, that **over-charges by exactly the open-handle
count** — committed bytes track one window however many handles are charged, so tightness is
`1/N` and a mount at 256 descriptors is charged 4295 MB while holding 17.8 MB, throttled 6–10×
for it (#298, #301).

The obvious next move is to loosen or remove it. **Don't.** Every cell behind that measurement
was a *single streaming reader*, so #55's thrash shape — many concurrent readers over a working
set larger than the memory tier — was never represented. This measures that shape.

## Phase 1b: production latency, real S3

`c7g.4xlarge` us-east-1, in-region reads of `s3://1000genomes/release/20130502/` (chr1…chr16,
1.0–1.25 GiB each), one `cat` per reader, fresh cold mount per cell, `--mem-cache 512MiB` held
constant. **~$0.13**, box terminated and verified.

One variable: whether prefetch is rationed. The divisor's output is
`clamp(budget/handles, 2, max-readahead)`, so a budget large enough that it saturates at
`max-readahead` for every N *is* "no rationing" — the memory tier then protects itself by
evicting alone.

| arm | N | wall | peak committed | % of budget | evicted unread | % of issued | S3 / working set |
|---|---|---|---|---|---|---|---|
| rationed (`--prefetch-budget 256MiB`) | 4 | **9.92 s** | 293.6 MB | 109.4% | **7** | 0.2% | **1.004×** |
| unrationed (`8GiB`) | 4 | 136.61 s | 1109.4 MB | 12.9% | 3656 | 81.2% | 4.204× |
| rationed | 8 | **9.19 s** | 310.4 MB | 115.6% | **2** | 0.0% | **1.001×** |
| unrationed | 8 | 205.56 s | 2207.3 MB | 25.7% | 7417 | 92.1% | 4.885× |
| rationed | 16 | **13.08 s** | 341.8 MB | 127.3% | **1** | 0.0% | **1.003×** |
| unrationed | 16 | 265.11 s | 3537.9 MB | 41.2% | 12702 | 91.3% | 5.174× |

Removing the rationing costs **13.8–22.4× wall clock, 4.2–5.2× the bytes, and 522–12702× the
unread evictions**, with **81–92% of all prefetch evicted before anything reads it**.

**The divisor is load-bearing. "Delete it" is off the table.**

### The N=8 and N=16 rationed nulls are NIC-suppressed; N=4 carries the finding

A NIC ceiling throttles *dispatch*, which suppresses thrash — so a clean `evicted_unread` can be
about the network rather than about eviction. Against this box's 7.5 Gbps baseline (938 MB/s):

| arm | N | achieved | % of baseline | |
|---|---|---|---|---|
| rationed | 4 | 481 MB/s | **51%** | clear of the ceiling |
| rationed | 8 | 927 MB/s | 99% | at the ceiling — null suspect |
| rationed | 16 | 1034 MB/s | 110% | at the ceiling — null suspect |
| unrationed | 4–16 | 146–263 MB/s | 16–28% | thrash-limited, not bandwidth-limited |

So the rationed arm's near-zero evictions at N=8 and N=16 cannot be distinguished from a
dispatch rate the NIC was already capping. **The N=4 pair is the interpretable one** — 51% of
baseline, 7 evictions against 3656, and 1.004× against 4.204× — and it carries the conclusion on
its own. The unrationed arms sitting at 16–28% of baseline is independent evidence they were
limited by re-fetch rather than by bandwidth.

Caveat raised by the reporting workload from having hit it in their own arm A, and it is the
reason to report fetch rate against NIC capacity beside any `evicted_unread` null.

## What the contrast actually isolates, which is not handle count

The unrationed arm's committed bytes peak at 1.1–3.5 GB — only **13–41% of its 8 GiB budget**,
yet 2–7× the **512 MiB tier**. The rationed arm sits at 109–127% of its 256 MiB budget and
0.6–0.7× the tier.

So the quantity that must be bounded is **committed against the tier**, and the budget (50% of
the tier by default) is a proxy for it that works. Setting the budget to 16× the tier removed
the proxy's meaning, which is what the unrationed arm demonstrates.

That reframes #301. The divisor's defect is not that it rations — rationing is essential — but
that it rations on **descriptor count**, which correlates with memory pressure only
accidentally. Both regimes are now measured:

| regime | working set vs tier | right answer |
|---|---|---|
| 3.78 GB object, 8.25 GB tier, 1 reader (#301) | fits | full depth; the floor of 2 is pure loss, 6–10× |
| 13.5 GB over a 512 MiB tier, 16 readers (here) | 26× over | the floor of 2 is **correct**; depth would thrash |

Handle count happens to separate those two cases in both measurements, which is why the divisor
has survived. It is not what distinguishes them.

## Phase 1a: the same contrast in a unit test, and why it was not enough

`TestPrefetchDivisorEarnsItsCost` (`internal/blockstore/divisorsweep_test.go`) runs the same two
arms against the fake server, extending `TestPrefetchBudgetNoThrash`'s fixture. It reproduces the
direction — at 16 readers the divisor cut unread evictions 41× (8 vs 332) and re-fetch from
1.648× to 1.016× — but **its memory pressure is not production's**:

| | committed / budget | committed / tier |
|---|---|---|
| production, N=1 | 45.3% | 22.7% |
| production, N=16 | 6.1% | 3.1% |
| fixture, N=4 | 109.2% | 54.6% |
| fixture, N=16 | 142.1% | 71.0% |

The fake returns instantly, so every dispatched block lands before consumption drains any — a
peak the network cannot produce. The test is kept as a cheap regression guard on the contrast,
with that limitation in its doc comment; the numbers above are the ones to quote.

## Reproducing

`p1b.sh` is the harness (it hard-codes nothing but the bucket); `p1b.log` is the raw output.
The per-cell metric samples are in the archived `w/peak.*` files.

```
go test ./internal/blockstore/ -run TestPrefetchDivisorEarnsItsCost -v   # Phase 1a, $0
scp p1b.sh <box>: && ./p1b.sh                                            # Phase 1b, ~$0.13
```

## Caveat on the window column in `p1b.log`

`win=` is a **max** over 100 ms samples, so it catches the transient while only the first
handle is open (`clamp(32/1, 2, 33) = 32`) rather than the steady-state per-handle window
(`32/16 = 2` at N=16). The eviction, byte and wall figures are totals and unaffected.
