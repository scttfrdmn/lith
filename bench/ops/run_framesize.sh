#!/usr/bin/env bash
# Frame-size curve (Step 3): pack A1 (1985 small files) at 16/8/4/1 MiB frames on
# cargoship v0.24.5; per frame size report archive size + frame count, tree-walk
# GETs/bytes, and a single 90 KB file cold random-read's bytes (over-fetch = the
# frame it pulls). Numbers via lith metrics (== the ledger, cross-checked s35).
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; mkdir -p "$MP"
LITH=/mnt/nvme/lith/bin/lith; BENCH=scttfrdmn-lith-bench
scr(){ curl -s http://127.0.0.1:9950/metrics 2>/dev/null; }
gv(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
bv(){ awk '$1=="lith_s3_bytes_total"{printf "%.1f",$2/1048576}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
for FS in 16 8 4 1; do
  P=lith-bench/cargoship/fs$FS
  MANK=$(s5cmd ls "s3://$BENCH/$P/uploads/*/manifest.json.gz" 2>/dev/null | awk '{print $NF}' | head -1)
  # archive size (sum of chunk objects) + frame count from the manifest
  s5cmd cat "s3://$BENCH/$P/uploads/$MANK" 2>/dev/null | gzip -dc > /tmp/fsm.json 2>/dev/null
  MANFULL="s3://$BENCH/$P/uploads/$MANK"
  read arch frames chunks < <(python3 -c "
import json;d=json.load(open('/tmp/fsm.json'))
print(sum(c.get('compressed_size',0) for c in d['chunks']), sum(len(c.get('frames',[])) for c in d['chunks']), len(d['chunks']))")
  "$LITH" index build --cargoship "$MANFULL" --index-file "$W/fs.idx" --region us-east-1 >/dev/null 2>&1
  src=$("$LITH" index inspect "$W/fs.idx" 2>/dev/null | awk '$1=="source:"{print $2}'); [ "$src" = cargoship ] || { echo "FS$FS: build not cargoship"; continue; }
  # tree walk
  um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/fs.idx" --metrics :9950 --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
  m0=$(scr); find "$MP" -type f -exec cat {} + >/dev/null 2>&1; m1=$(scr)
  wg=$(( $(gv "$m1") - $(gv "$m0") )); wb=$(awk -v a=$(bv "$m0") -v b=$(bv "$m1") 'BEGIN{printf "%.1f",b-a}')
  # single-file cold random read: pick one file, fresh mount
  um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/fs.idx" --metrics :9950 --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
  F1=$(find "$MP" -type f | sort | sed -n '900p'); m0=$(scr); cat "$F1" >/dev/null 2>&1; m1=$(scr)
  sg=$(( $(gv "$m1") - $(gv "$m0") )); sb=$(awk -v a=$(bv "$m0") -v b=$(bv "$m1") 'BEGIN{printf "%.2f",b-a}')
  um
  awk -v fs=$FS -v a="$arch" -v fr="$frames" -v ch="$chunks" -v wg=$wg -v wb="$wb" -v sg=$sg -v sb="$sb" \
    'BEGIN{printf "FSCURVE|frame=%dMiB|archive_MB=%.1f|frames=%d|chunks=%d|walk_gets=%d|walk_MB=%s|1file_gets=%d|1file_MB=%s\n",fs,a/1048576,fr,ch,wg,wb,sg,sb}'
done
