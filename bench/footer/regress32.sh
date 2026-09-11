#!/usr/bin/env bash
# Session-32 regression block: prove #122 (feat/sparse, --footer-tier2 default OFF) is
# within noise of base on R4 (Overture, default streaming Parquet path) and on the
# 1000genomes CRAM prefetch scenarios (S1 sequential, 8-reader, R2 random).
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; LOG=$W/reg32.log
exec > >(tee -a "$LOG") 2>&1
echo "===== reg32 $(date -u) ====="
BB=/mnt/nvme/lith/bin/lith-base; BP=/mnt/nvme/lith/bin/lith-post
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
mgets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mb(){ awk -v b="${1:-0}" 'BEGIN{printf "%.0f",b/1048576}'; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
MP=/mnt/nvme/mp; mkdir -p "$MP"
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith-base 2>/dev/null; pkill -x lith-post 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }

# ---------- R4 (Overture staged; post mounts with tier2 default OFF) ----------
mapfile -t OV < "$W/ov8.txt"
BKT=scttfrdmn-lith-bench; REL=lith-bench/overture-places; PREFIX=s3://$BKT/$REL
IDX=$W/r.idx
r4mount(){ um; rm -f "$IDX"; drop; "$1" mount "$PREFIX" "$MP" --index-file "$IDX" --metrics ":$2" --daemon
  for i in $(seq 1 100); do mountpoint -q "$MP" && break; sleep 0.5; done; mountpoint -q "$MP"||{ echo MFAIL; return 1; }; }
# read the projection columns of one parquet file by streaming it (default path): a full
# read via dd is the fair "app reads the file" proxy without pyarrow.
r4read(){ dd if="$MP/${OV[0]}" of=/dev/null bs=8M 2>/dev/null; }
r4single(){ local label=$1 bin=$2 port=$3 c=()
  for r in 1 2; do r4mount "$bin" "$port" || return 1; local t0; t0=$(now); r4read; local t1; t1=$(now); c+=("$(el "$t0" "$t1")")
    [ "$r" = 1 ] && m=$(scrape "$port"); um; done
  echo "REG_R4S|$label|cold=${c[0]},${c[1]}|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; }
r4conc(){ local label=$1 bin=$2 port=$3; r4mount "$bin" "$port" || return 1
  local t0; t0=$(now); pids=(); for i in 0 1 2 3 4 5 6 7; do dd if="$MP/${OV[$i]}" of=/dev/null bs=8M 2>/dev/null & pids+=($!); done
  for p in "${pids[@]}"; do wait "$p"; done; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG_R4C|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }

# ---------- CRAM (1000genomes public; prebuilt phase3 index) ----------
mapfile -t KEYS < "$W/fanout-64.txt"
crmount(){ um; drop; "$1" mount s3://1000genomes "$MP" --index-file "$W/p3.idx" --no-sign-request --metrics ":$2" --daemon
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done; mountpoint -q "$MP"||{ echo MFAIL; return 1; }; }
relpath(){ echo "$MP/${1#phase3/data/}"; }
# S1: sequential stream of one CRAM (exercises the readahead/BDP window, #56).
# 8-reader: 8 CRAMs streamed concurrently (fan-out + prefetch budget). R2: random 4k reads.
# Reads via dd, not samtools: the regression probes lith's I/O paths (GETs/bytes/wall), and
# CRAM decode is compute-bound and would mask any lith-side difference between base and post.
cr_s1(){ local label=$1 bin=$2 port=$3; crmount "$bin" "$port" || return 1
  local f; f=$(relpath "${KEYS[0]}"); local t0; t0=$(now); dd if="$f" of=/dev/null bs=8M 2>/dev/null; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG_S1|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }
cr_r8(){ local label=$1 bin=$2 port=$3; crmount "$bin" "$port" || return 1
  local t0; t0=$(now); pids=(); for i in 0 1 2 3 4 5 6 7; do dd if="$(relpath "${KEYS[$i]}")" of=/dev/null bs=8M 2>/dev/null & pids+=($!); done
  for p in "${pids[@]}"; do wait "$p"; done; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG_R8|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }
cr_r2(){ local label=$1 bin=$2 port=$3; crmount "$bin" "$port" || return 1
  local f; f=$(relpath "${KEYS[0]}")
  local sz; sz=$(stat -c %s "$f" 2>/dev/null); local blk=$((sz/4096))
  # 200 random 4 KiB reads scattered across the object — exercises the random-read
  # detector / readahead-collapse path (#56); measures GETs + wall, not CRAM semantics.
  local t0; t0=$(now)
  for i in $(seq 1 200); do local off=$(( (RANDOM*RANDOM) % (blk>0?blk:1) )); dd if="$f" of=/dev/null bs=4096 skip="$off" count=1 2>/dev/null; done
  local t1; t1=$(now); m=$(scrape "$port")
  echo "REG_R2|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }

echo "--- R4 (Overture; post = tier2 default off) ---"
r4single "base" "$BB" 9811
r4single "post" "$BP" 9812
r4conc   "base" "$BB" 9813
r4conc   "post" "$BP" 9814
echo "--- CRAM S1 / 8-reader / R2 (1000genomes) ---"
cr_s1 "base" "$BB" 9821; cr_s1 "post" "$BP" 9822
cr_r8 "base" "$BB" 9823; cr_r8 "post" "$BP" 9824
cr_r2 "base" "$BB" 9825; cr_r2 "post" "$BP" 9826
echo "===== reg32 DONE $(date -u) ====="
grep -E '^REG_' "$LOG"
