# Meet a deadline

You have more work than one node can finish in time. Go wider.

## Why wider is also cheaper with a mount

With a **copy-based** data plane, going wider costs more: each of your N nodes
either stages its own copy (N × the staging bill and wall-clock) or you stand up
a shared filesystem sized and paid for per job. Width multiplies the data-plane
cost.

With a **mount**, the arithmetic changes. The compute is the same total
node-hours at any width — ten nodes for one hour is the same as one node for ten
hours — and the data cost is **bytes touched**, which is fixed by the work, not
by how many nodes touch them. So there is no width penalty on data, and
finishing **sooner** and finishing **cheaper** stop being a trade-off: they are
the same choice.

<!-- number: session 17 — fan-out chart: total $ flat across N (mount) vs rising (copy); wall-clock falling as 1/N -->

*(A measured fan-out chart — cost flat across node count for the mount, cost
rising for the copy path, wall-clock falling as 1/N — lands here after the
session-17 run.)*

## How to do it

Fan out with an instance array, one lith mount per node, each node reading its
own shard by key range. No shared state, so nothing coordinates and nothing
contends:

```bash
# One node's job, parameterized by shard index (0..N-1) of N.
# The index defines a key range; the node mounts and processes only its shard.
lith mount s3://your-bucket/dataset /mnt/data \
  --index-file /tmp/dataset.lithidx --daemon
process-shard /mnt/data --shard "$SHARD_INDEX" --of "$N_SHARDS"
fusermount3 -u /mnt/data
```

Launch N of them as an array (e.g. `spawn array`), each with its own
`SHARD_INDEX`. Points that make this cheap and safe:

- **Shard by key range, not by copy.** Each node's `process-shard` walks the
  keys assigned to its index. lith serves every node's metadata from the local
  index and fetches only the objects that node reads.
- **No shared filesystem.** There is nothing to provision, size, or tear down
  between the nodes; each mount is independent and local to its node.
- **Spot-safe.** Nothing durable lives on the node — no staged copy to lose — so
  a reclaimed spot instance costs only its in-flight shard, which the array
  reschedules. This is what makes width cheap: you can run the whole fan-out on
  interruptible capacity.
- **Build the index once, reuse it N times.** Build the `--index-file` in the
  launcher and hand the same file to every node (or let each node auto-build for
  a small prefix); the listing cost is paid once, not per node.

Prebuild the index in your launcher, distribute it with the job, and each node
starts reading immediately with zero metadata round-trips.

See your spore.host cookbook's **job-arrays** pattern for the launcher side
(array submission, per-index parameters, spot retries).
<!-- link: cookbook job-arrays URL pending -->
