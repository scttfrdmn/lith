# Start here

Five minutes from nothing to a real answer, against a public Registry of Open
Data bucket. You need a Linux box (lith uses FUSE) and `samtools` for the last
step. Nothing is written to the bucket; nothing but an index file is written
locally.

*Why "lith"?* As in *lithic* / *lithology* — rock strata. lith serves a bucket's
objects as read-only strata, exactly as they were laid down; the bucket's native
key layout is the stratum, and lith never rewrites it.

## 1. Install

Download the static binary for your architecture from the
[releases page](https://github.com/scttfrdmn/lith/releases):

```bash
# arm64 (Graviton, Apple-on-Linux VMs, …)
curl -fsSL https://github.com/scttfrdmn/lith/releases/latest/download/lith_linux_arm64 -o lith
# or x86-64
curl -fsSL https://github.com/scttfrdmn/lith/releases/latest/download/lith_linux_amd64 -o lith
chmod +x lith && sudo mv lith /usr/local/bin/
lith version
```

Or, with Go installed: `go install github.com/scttfrdmn/lith/cmd/lith@latest`.

## 2. Build the index

`s3://1000genomes` is public, so pass `--no-sign-request`. Index one sample's
alignment folder — a handful of keys, so the build is instant:

```bash
lith index build s3://1000genomes/phase3/data/HG00100/alignment \
  --no-sign-request --index-file /tmp/hg00100.lithidx
lith index inspect /tmp/hg00100.lithidx
```

`inspect` prints the key count and a few stats. This listing is the only time
lith calls S3 for metadata; from here `ls` and `stat` are answered locally.

## 3. Mount it

```bash
sudo mkdir -p /mnt/hg00100
lith mount s3://1000genomes/phase3/data/HG00100/alignment /mnt/hg00100 \
  --index-file /tmp/hg00100.lithidx --no-sign-request --daemon
ls -lh /mnt/hg00100
```

`ls` returns immediately and shows a **14 GiB** `…low_coverage…bam.cram` and its
`.crai` index — real sizes and mtimes, served from the local index without
touching S3.

## 4. Ask a question

Count the reads in a 1 Mb slice of chromosome 20. samtools seeks into the CRAM
through the mount; lith fetches only the bytes those reads occupy (it fetches
the CRAM reference from the public EBI registry automatically on first use, so
this step needs outbound internet):

```bash
samtools view -c /mnt/hg00100/HG00100.*.low_coverage*.cram 20:1000000-2000000
```

You get a count in a second or two, having moved on the order of tens of MB —
not the whole 14 GiB file.

## 5. Measure the read path

`lith bench` reads an object through a mount and reports throughput, S3
requests, and an estimated cost, cold then warm:

```bash
lith bench s3://1000genomes/phase3/data/HG00100/alignment/HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram \
  --pattern seq --no-sign-request
```

## 6. Unmount

```bash
lith umount /mnt/hg00100
```

`lith umount` signals the mount process and waits for it to leave the mount
table (falling back to `fusermount3 -u` if needed); `lith mounts` lists every
live lith mount. You can still `fusermount3 -u` by hand.

## Mount at any prefix

The `s3://bucket/prefix` you pass to `mount` becomes the **root** of the
filesystem — above, `/mnt/hg00100` is rooted at `phase3/data/HG00100/alignment/`,
so its top-level entries are that folder's contents, not the whole bucket. One
prebuilt index can back many prefix mounts at once: build a wide index once and
mount any prefix at or below its root, concurrently, with no rebuild —

```bash
lith index build s3://1000genomes/phase3/data --no-sign-request --index-file /tmp/phase3.lithidx
lith mount s3://1000genomes/phase3/data/HG00096 /mnt/hg00096 --index-file /tmp/phase3.lithidx --no-sign-request --daemon
lith mount s3://1000genomes/phase3/data/HG00100 /mnt/hg00100b --index-file /tmp/phase3.lithidx --no-sign-request --daemon
lith umount --all
```

## What just happened

- You **listed once.** The index build was the single metadata pass; every
  `ls`, `stat`, and `open` after it was served locally with zero S3 calls.
- You **read only what you touched.** The region query answered a question about
  a 14 GiB object by pulling a small fraction of it — no staging, no copy, no
  volume to size or delete.
- You **wrote nothing to the bucket.** lith is read-only by construction; the
  only local artifact is the index file you named.

Whether that trade — no staging, bytes-you-touch — actually beats copying the
file first depends on your access shape and your box. That is the next page:
**[Copy or mount?](copy-or-mount.md)**
