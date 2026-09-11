#!/usr/bin/env bash
# Session-32 CRAM regression (ruling): isolate #122's cache-unit change on the
# non-footer read path. base = pre-#122 (ddba4d8), post = main @ c337973 (+#122).
# Rebuilds a v4 index (the v2 p3.idx no longer opens), then runs the canonical
# S1 (flagstat), 8-reader (8x flagstat), R2 (samtools view -c region seeks).
# I/O metrics (GETs, S3 bytes) are primary; hold if any moves >5%.
set -uo pipefail
export AWS_REGION=us-east-1
export REF_PATH='https://www.ebi.ac.uk/ena/cram/md5/%s'   # b37 ref slices for CRAM decode
W=/mnt/nvme/work; LOG=$W/reg33.log
exec > >(tee -a "$LOG") 2>&1
echo "===== reg33 $(date -u) ====="
BB=/mnt/nvme/lith/bin/lith-base; BP=/mnt/nvme/lith/bin/lith-post
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
mgets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mb(){ awk -v b="${1:-0}" 'BEGIN{printf "%.0f",b/1048576}'; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/p3v4.idx
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith-base 2>/dev/null; pkill -x lith-post 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
mapfile -t RAW < "$W/fanout-64.txt"
KEYS=(); for l in "${RAW[@]}"; do [[ "$l" == \#* ]] && continue; KEYS+=("${l%%$'\t'*}"); done

# 100 chr20 1 Mb region seeks (b37), one samtools call.
REGIONS=""
for i in $(seq 0 99); do s=$((i*600000+1)); e=$((s+999999)); REGIONS="$REGIONS 20:$s-$e"; done

mnt(){ um; drop; "$1" mount s3://1000genomes "$MP" --index-file "$IDX" --no-sign-request --metrics ":$2" --daemon
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done; mountpoint -q "$MP"||{ echo MFAIL; return 1; }; }

s1(){ local label=$1 bin=$2 port=$3; mnt "$bin" "$port" || return 1
  local f="$MP/${KEYS[0]}"; local t0; t0=$(now); samtools flagstat "$f" >/dev/null 2>&1; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG33_S1|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }
r8(){ local label=$1 bin=$2 port=$3; mnt "$bin" "$port" || return 1
  local t0; t0=$(now); pids=(); for i in 0 1 2 3 4 5 6 7; do samtools flagstat "$MP/${KEYS[$i]}" >/dev/null 2>&1 & pids+=($!); done
  for p in "${pids[@]}"; do wait "$p"; done; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG33_R8|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }
r2(){ local label=$1 bin=$2 port=$3; mnt "$bin" "$port" || return 1
  local f="$MP/${KEYS[0]}"; local t0; t0=$(now); samtools view -c "$f" $REGIONS >/dev/null 2>&1; local t1; t1=$(now); m=$(scrape "$port")
  echo "REG33_R2|$label|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"; um; }

s1 "base" "$BB" 9841; s1 "post" "$BP" 9842
r8 "base" "$BB" 9843; r8 "post" "$BP" 9844
r2 "base" "$BB" 9845; r2 "post" "$BP" 9846
echo "===== reg33 DONE $(date -u) ====="
grep -E '^REG33' "$LOG"
