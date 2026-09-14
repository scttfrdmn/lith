# Published datasets

Pack and publish once; mount it anywhere by name.

A **published dataset** is a prefix that CargoShip's `publish` writes: immutable
versions plus one atomic pointer. lith mounts it by name — no index file to track,
no manifest URL to remember.

```
s3://bucket/dataset/
  CURRENT                      <- pointer: JSON naming the current version
  v/20260912T1830Z-ab12cd/     <- an immutable version
    index.lith                 <- the lith index (built by publish)
    uploads/<id>/…             <- the CargoShip archive chunks + manifest
```

## Producing one

```
cargoship publish /data s3://bucket/dataset --frame-size 4MiB
```

`publish` packs the tree, builds the lith index inline, writes the immutable
version directory, and flips `CURRENT` **last** — after every prerequisite object
is verified durable. A reader sees the old version or the new one, never a mix;
nothing is deleted by publish. Publishing one version of a 1,985-file tree costs a
handful of PUTs (measured: 3 chunks + manifest + index + `CURRENT` = 6).

Related commands: `cargoship publish list` (versions, which is current),
`cargoship publish promote <version> …` (flip `CURRENT` to an existing version),
`cargoship publish prune --keep N` (explicit retention — never the current
version, never chunks a retained version still needs).

## Mounting one

```
lith mount s3://bucket/dataset@current /mnt/data     # the current version
lith mount s3://bucket/dataset@20260912T1830Z-ab12cd /mnt/data   # pin a version
```

`@current` resolves the pointer and mounts that version's prebuilt index; a bare
`s3://bucket/prefix` (no `@`) mounts the native layout exactly as before.
Mounting `@current` costs one GET more than mounting the archive's manifest
directly — the `CURRENT` read — and fetches identical bytes (measured: 23 vs 22
GETs, 13.1 MB either way, byte-identical).

**Mount by name from anywhere.** Two readers on two machines that both mount
`@current` resolve the same version and serve byte-identical namespaces with no
coordination between them — that is what makes it a *published dataset* rather
than an archive with a pointer file beside it.

### Fail closed

The `CURRENT` pointer is attacker-controlled bytes from a bucket lith does not
own. A missing, malformed, or dangling pointer — or one naming an index that does
not exist — is a hard error naming the pointer key; lith **never** falls back to
listing the bucket. A pointer whose index came from a chunkless (unmountable)
archive is refused rather than mounted as an empty namespace.

## Adopting a new version: `lith refresh`

```
lith refresh /mnt/data
```

`refresh` re-reads `CURRENT` and, if a new version was published, **atomically
swaps** the mount to it. It is explicit — lith never polls. Open file handles keep
the version they were opened against (versions are immutable, so their objects
still exist), so a read in flight completes consistently; only new lookups see the
new version. Inodes are content-addressed by path, so a file present in both
versions keeps its inode across the swap — a cached `(dev, ino)` in `find` or
`rsync` stays valid.

Resolving to the version already mounted is a no-op success, not an error.

## The gateway is different: restart to adopt

The NFS gateway (`lith serve nfs s3://bucket/dataset@current`) does **not**
hot-swap, and this is deliberate. NFSv3 file handles embed the served version's
identity, so adopting a new version would change every handle and **STALE every
connected client** — and NFSv3 is stateless, so lith cannot tell whether clients
are connected. So a version-changing `refresh` on the gateway is **refused** with
a message telling you to **restart the gateway and remount clients** to adopt the
new version. (A refresh that resolves to the version already being served is a
no-op.)

| | FUSE mount | NFS gateway |
|---|---|---|
| `refresh` to a new version | hot-swaps in place | **refused** — restart to adopt |
| in-flight reads across a change | complete on the old version | (no live change) |
| clients | one, local | many, over the network |

This asymmetry is the honest consequence of the two protocols: a single local
kernel client can be swapped under safely; a stateless multi-client protocol
cannot, so lith refuses rather than silently staling every mount.
