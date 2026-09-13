# Serving a cluster

One node mounts; a cluster shares. `lith serve nfs` turns a single node into a
**read-only NFSv3 gateway**: it holds the index and the cache and serves N
compute nodes, so the cluster reads a dataset from S3 through **one** node
instead of N.

```
# on the gateway (c8gd.4xlarge with local NVMe for the disk cache)
lith serve nfs s3://bucket/prefix --index-file dataset.idx \
  --disk-cache 100GB --disk-path /mnt/nvme/cache --listen :2049

# on each compute node
sudo mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,\
rsize=1048576,wsize=1048576,nconnect=4,actimeo=600 <gateway-ip>:/ /mnt/data
```

`--disk-cache` is not optional for a gateway: size it to the working set so a
re-run and a restart serve from local disk instead of re-fetching S3.

## The one rule: does the cluster share data?

The gateway is a **funnel** — every client's bytes cross one node's NIC. That
shapes the entire decision:

- **Distinct data per node** (each node reads a different object — a per-sample
  BAM, one shard each): **give each node its own `lith mount`.** N nodes on N
  NICs read in parallel; a shared cache has nothing to share. The gateway would
  only serialize them behind one NIC.
- **Shared data across nodes** (all nodes read the same reference, index, or
  model — the bwa/STAR index, a shared Zarr, a foundation-model checkpoint):
  **use the gateway.** It fetches the shared dataset from S3 **once** and serves
  it to all N nodes, and the **second job is free**.

### Measured, N=8, ledger-backed

| workload | gateway | independent `lith mount` | takeaway |
|---|---|---|---|
| **distinct** (8 CRAMs, one per node, `flagstat`) | 90 s, 6,762 GET | **72 s, 7,183 GET** | independent wins — no sharing to exploit |
| **shared** (8 nodes read the same 8.88 GB bwa index) | 36 s, **1,139 GET / 9.1 GB** | 13 s, 8,561 GET / 71.3 GB | gateway moves **1/8 the S3 traffic**; independent is faster cold |
| **shared, second run** | **0.4 s, 0 GET** | (re-fetches 71 GB) | the free second job |

On shared data the gateway reads the dataset from S3 **once** (1,139 GETs / 9.1 GB)
where eight independent mounts read it **eight times** (8,561 GETs / 71.3 GB) —
the "N nodes, one node's S3 traffic" claim, quantified at ~1/8. It is **not**
faster cold (the funnel: one NIC vs eight), but it is far cheaper on S3 traffic
and rate-limit pressure, and a re-run costs **zero** S3.

## Sizing the gateway

Size it by **aggregate demand**, not one client's: the gateway's NIC and the
disk cache are shared by all N nodes. A `c8gd.4xlarge` (NVMe, ~15 Gbps) serves a
handful of nodes reading a shared working set comfortably; scale the instance up
with N and the working set. The gateway's own read path is concurrent and has
**no FUSE hop** — a single stream reads *faster* than a FUSE mount (1,463 MB/s
loopback) — but aggregate cold throughput is still bounded by the one NIC.

## When the gateway dies

Reads are served over NFS, so the client's mount options decide behavior:

- **`hard`** (recommended for data integrity): clients **block** while the
  gateway is down and **resume** transparently when it restarts. No error, no
  corruption — the correct default.
- **`soft`**: clients get a clean **I/O error** after the timeout, so a job fails
  fast instead of hanging.

Restart is cheap: the gateway reloads the index **from the file** (no
re-listing) and serves the working set from its **on-disk cache** — a restart
mid-job re-serves with **~0 S3 GETs**.

## Versus EFS and FSx Lustre

The gateway **replaces a managed shared filesystem** for read-only S3 datasets,
without one's setup:

- **EFS** must be **hydrated** from S3 first — measured **~4.5 min to copy an
  8.88 GB index** (~32 MB/s), during which compute idles, and its shared read was
  the slowest of the three. Then you pay EFS storage + throughput and delete it.
- **FSx Lustre** has a **1.2 TiB minimum** (you pay for 1.2 TiB to use 9 GB) and
  lazy-loads from S3 on first read (slow); its Linux client is also unavailable
  on some platforms (arm64 Ubuntu).
- **The gateway** hydrates **nothing** — it serves directly from S3 on the first
  read, has no filesystem minimum, and is a single process to start and stop.

For a shared, read-only S3 dataset across a cluster, the gateway is the cheapest
and simplest of the three; for read-write scratch or POSIX-complete semantics,
EFS/FSx remain the right tools.

## Limits (v0.4)

NFSv3 only (no NFSv4 state/delegations/ACLs); **read-only** (every mutating op is
`NFS3ERR_ROFS`); `AUTH_UNIX` with squash, **no Kerberos**; file handles are
`(index-sha, inode)` — stable across a restart against the same index, `STALE`
against a rebuilt one; per-**path** (not per-client) sequential state, a
limitation of go-nfs's stateless read path that does not affect throughput or the
measured fair-share spread.
