#!/usr/bin/env bash
# Step-0 diagnosis for #137: split the a1-244 (multi-chunk) walk cost.
# Cats files in tree-order batches, snapshotting lith metrics between, to see
# whether per-fill frame refetch (cause A) or a multi-chunk discontinuity
# (cause B) dominates. $1 = a1-244 manifest URL.
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; mkdir -p "$MP"
LITH=/mnt/nvme/lith/bin/lith
MAN="$1"
scr(){ curl -s http://127.0.0.1:9930/metrics 2>/dev/null; }
g(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
b(){ awk '$1=="lith_s3_bytes_total"{printf "%.1f",$2/1048576}' <<<"$1"; }
fr(){ awk '$1=="lith_backing_frames_fetched_total"{print $2+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
"$LITH" index build --cargoship "$MAN" --index-file "$W/d.idx" --region us-east-1 >/dev/null 2>&1
um; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
"$LITH" mount s3://scttfrdmn-lith-bench "$MP" --index-file "$W/d.idx" --metrics :9930 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
mapfile -t F < <(find "$MP" -type f | sort)
echo "total files: ${#F[@]}"
delta(){ # name  n-files-to-cat-from-index $2..
  local name=$1; shift
  local m0; m0=$(scr); local g0=$(g "$m0") b0=$(b "$m0") f0=$(fr "$m0")
  for f in "$@"; do cat "$f" >/dev/null 2>&1; done
  local m1; m1=$(scr); local g1=$(g "$m1") b1=$(b "$m1") f1=$(fr "$m1")
  awk -v n=$# -v g=$((g1-g0)) -v f=$((f1-f0)) -v b0="$b0" -v b1="$b1" -v nm="$name" \
    'BEGIN{printf "DIAG|%s|files=%d|gets=%d|frames=%d|MB=%.1f|MB_per_file=%.2f\n",nm,n,g,f,b1-b0,(b1-b0)/n}'
}
# batch 1: first 50 files (all in chunk-0, span ~1-2 frames)
delta "first50-chunk0" "${F[@]:0:50}"
# batch 2: next 50 files (still chunk-0)
delta "next50-chunk0" "${F[@]:50:50}"
# batch 3: single file re-read (cache hit test) — cat F[0] again
delta "reread-file0" "${F[0]}"
# batch 4: the rest (crosses into chunk-1/chunk-2)
delta "rest-all" "${F[@]:100}"
um
