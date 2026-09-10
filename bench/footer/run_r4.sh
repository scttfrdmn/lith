#!/usr/bin/env bash
# R4 (Overture places projection) — footer criterion 3. Appends RESULT lines.
set -uo pipefail
W=/mnt/nvme/work; LOG=$W/r4.log
exec > >(tee -a "$LOG") 2>&1
echo "===== r4 $(date -u) box=$(hostname) ====="
PY=/mnt/nvme/venv/bin/python
Q=/tmp/ft/pyquery.py
PREFIX=s3://overturemaps-us-west-2/release/2026-08-19.0/theme=places/type=place
REL=release/2026-08-19.0/theme=places/type=place
BKT=overturemaps-us-west-2
FILES=(part-00000-c7e47654-8483-5b8f-b183-7ba73334f7a5-c000.zstd.parquet
part-00001-01525d53-9fbf-5f59-aa2a-c557934aeb8a-c000.zstd.parquet
part-00002-06d0251d-44ae-5400-ab29-cb4457570b0d-c000.zstd.parquet
part-00003-9e8cf04e-9fcc-5346-af85-9883ac4821d8-c000.zstd.parquet
part-00004-e1b1066c-7a59-5692-b21d-7e03fafaf0a4-c000.zstd.parquet
part-00005-c7ae3183-76f1-5b61-bf21-1bc92b346aff-c000.zstd.parquet
part-00006-b9b7213b-ab21-565f-b7f2-a76b5761049c-c000.zstd.parquet
part-00007-738c130e-9a4b-5d01-b521-a3d872c8cef9-c000.zstd.parquet)

MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/r4.idx
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
mgets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
IFACE=$(ip route get 1.1.1.1 2>/dev/null | grep -oP "dev \K\S+" | head -1)
rx(){ cat /sys/class/net/$IFACE/statistics/rx_bytes; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
umount_mp(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
mount_fresh(){ # bin extra port
  umount_mp; rm -f "$IDX"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$1" mount "$PREFIX" "$MP" --index-file "$IDX" --no-sign-request --region us-west-2 --metrics ":$3" --daemon $2
  for i in $(seq 1 80); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo MOUNT_FAIL; return 1; }
}

# ---- single-process: base / tier1 / tier1+2, 3 cold + 1 warm ----
single(){ # label bin extra port
  local label=$1 bin=$2 extra=$3 port=$4 colds=() warm m0 m1 sb gets dist ranges
  mount_fresh "$bin" "$extra" "$port" || return 1; "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; umount_mp # warmup ref (not timed; lith cache reset next)
  for rep in 1 2 3; do
    mount_fresh "$bin" "$extra" "$port" || return 1
    [ "$rep" = 1 ] && m0=$(scrape "$port")
    local t0; t0=$(now); "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; local t1; t1=$(now)
    colds+=("$(el "$t0" "$t1")")
    if [ "$rep" = 1 ]; then
      m1=$(scrape "$port"); sb=$(mval "$m1" lith_s3_bytes_total); gets=$(mgets "$m1")
      dist=$(mval "$m1" lith_distinct_bytes_read); ranges=$(awk '/^lith_format_plan_ranges_total\{format="parquet"/{s+=$2} END{print s+0}' <<<"$m1")
      local w0; w0=$(now); "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; local w1; w1=$(now); warm=$(el "$w0" "$w1")
    fi
    umount_mp
  done
  echo "R4SINGLE|$label|cold=${colds[0]},${colds[1]},${colds[2]}|warm=$warm|s3_MB=$(awk -v b="${sb:-0}" 'BEGIN{printf "%.1f",b/1048576}')|gets=${gets:-0}|distinct_MB=$(awk -v b="${dist:-0}" 'BEGIN{printf "%.1f",b/1048576}')|plan_ranges=${ranges:-0}"
}

# ---- 8 concurrent, 2 cold ----
conc_lith(){ # label bin extra port
  local label=$1 bin=$2 extra=$3 port=$4
  for rep in 1 2; do
    mount_fresh "$bin" "$extra" "$port" || return 1
    local t0; t0=$(now); pids=()
    for i in 0 1 2 3 4 5 6 7; do "$PY" "$Q" local "$MP/${FILES[$i]}" >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); m1=$(scrape "$port")
    echo "R4CONC|$label|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(awk -v b="$(mval "$m1" lith_s3_bytes_total)" 'BEGIN{printf "%.1f",b/1048576}')|gets=$(mgets "$m1")"
    umount_mp
  done
}
conc_pyarrow(){
  for rep in 1 2; do
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local r0; r0=$(rx); local t0; t0=$(now); pids=()
    for i in 0 1 2 3 4 5 6 7; do "$PY" "$Q" s3 "$BKT/$REL/${FILES[$i]}" >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); local r1; r1=$(rx)
    echo "R4CONC|pyarrow-native|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(awk -v a="$r0" -v b="$r1" 'BEGIN{printf "%.1f",(b-a)/1048576}')(rx)|gets=NA"
  done
}
single_pyarrow(){
  local colds=()
  for rep in 1 2 3; do
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local r0; r0=$(rx); local t0; t0=$(now); "$PY" "$Q" s3 "$BKT/$REL/${FILES[0]}" >/dev/null 2>&1; local t1; t1=$(now); local r1; r1=$(rx); colds+=("$(el "$t0" "$t1")"); rxmb=$(awk -v a="$r0" -v b="$r1" 'BEGIN{printf "%.1f",(b-a)/1048576}')
  done
  echo "R4SINGLE|pyarrow-native|cold=${colds[0]},${colds[1]},${colds[2]}|warm=NA|s3_MB=$rxmb(rx)|gets=NA|distinct_MB=NA|plan_ranges=NA"
}

BM=/mnt/nvme/lith/bin/lith-main
BF=/mnt/nvme/lith/bin/lith-footer
single "base"      "$BM" ""                    9401
single "tier1"     "$BF" "--footer-tier2=false" 9402
single "tier1+2"   "$BF" ""                     9403
single_pyarrow
conc_lith "base"    "$BM" ""  9404
conc_lith "tier1+2" "$BF" ""  9405
conc_pyarrow
echo "===== r4 DONE $(date -u) ====="
grep -E '^R4' "$LOG"
