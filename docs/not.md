# What lith is not

Knowing the edges keeps "matches or beats a copy" from turning into a
disappointment.

## By design

- **Not writable.** There is no write path. Every mutating operation —
  `write`, `create`, `truncate`, `rename`, `unlink`, `chmod` — returns `EROFS`.
  This is not "writes disabled"; the write path is *absent*. lith is for reading
  data that lives in S3, not for editing it.
- **Linux only** (for v0.x). lith relies on FUSE; macOS and Windows are out of
  scope.
- **No time travel.** lith reads the current object for each key. It does not
  expose S3 object versions or let you mount the bucket as of a past time; if a
  key's ETag has changed since the index was built, that read returns `EIO`
  (stale), and you rebuild or `lith index refresh`.
- **No cache encryption.** The memory and optional disk cache tiers hold
  plaintext object bytes. Don't point `--disk-cache` at shared or untrusted
  storage; on a multi-tenant box, treat the cache as you would any local scratch
  of the data.
- **The index is a private cache, not a bucket artifact.** lith writes **nothing
  to the bucket** — no manifest, no sidecar, no marker objects. The
  `--index-file` is yours, local, and disposable; delete it and rebuild anytime.
  Another producer can own the bucket and never know lith read it.

## Physical limits it cannot beat

lith removes staging and serves metadata locally, but it still reads from S3
over a network. It cannot beat:

- **S3 time-to-first-byte, per object.** The first byte of each distinct object
  costs a round-trip. For one large object this is amortized instantly; for
  **many small objects read in key order** (Zarr, sharded stores) it is the cost
  that matters, and lith pays it per object unless prefetch can lead far enough
  ahead (the [#63](https://github.com/scttfrdmn/lith/issues/63) /
  [#70](https://github.com/scttfrdmn/lith/issues/70) work).
- **The NIC.** Cold throughput is bounded by the instance's network bandwidth —
  size the box for it ([Sizing the node](sizing.md)).
- **Per-connection S3 throughput.** A single stream tops out well below the NIC;
  lith fans reads across many connections to fill the link, but no single object
  read goes faster than S3 will serve one connection.
- **The application's CPU.** When the job is CPU-bound (decoding CRAM, parsing),
  the bytes arrive faster than the app consumes them, and wall-clock is set by
  the CPU — where a local copy on the same box ties, and lith wins only by not
  having staged first.

When lith "matches or beats a copy," it is bounded by these; the win comes from
removing staging and moving only the bytes you touch, not from outrunning
physics.
