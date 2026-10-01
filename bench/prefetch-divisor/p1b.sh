#!/usr/bin/env bash
# PHASE 1b of #301: does the prefetch-budget divisor earn its cost at PRODUCTION memory
# pressure? Phase 1's fake-server sweep showed the divisor cuts unread evictions 41x at 16
# readers -- but with peak committed at 109-142% of budget, where a real mount sits at
# 0.4-45%. The fake returns instantly, so dispatch outruns consumption in a way the network
# cannot. This repeats the contrast where the latency is real.
#
# The two arms, one variable apart: whether prefetch is RATIONED. The divisor's output is
# clamp(budget/handles, 2, max-readahead), so making the budget large enough that it
# saturates at max-readahead for every N is exactly "no rationing" -- the memory tier alone
# then protects itself by evicting. Same tier, same objects, same readers in both.
set -uo pipefail
MP=/mnt/mp; W=/tmp/w; PORT=9801; mkdir -p "$W"
P=s3://1000genomes/release/20130502
REG="--region us-east-1 --no-sign-request"
TIER=512MiB           # memory tier, far below the combined working set
RATIONED=256MiB       # the shipping 1:2 budget-to-tier ratio
UNRATIONED=8GiB       # divisor saturates at --max-readahead for every N tested

OBJS=()
for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16; do
  OBJS+=("ALL.chr${n}.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz")
done

sv(){ awk -v k="$2" 'index($0,k)==1 {s+=$NF} END{printf "%.0f", s+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }

cell(){ # arm readers budget
  local arm=$1 n=$2 budget=$3
  um; rm -f "$W/idx"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  /tmp/lith mount "$P" "$MP" --index-file "$W/idx" $REG --nic-gbps 7.5 \
    --mem-cache "$TIER" --prefetch-budget "$budget" --metrics ":$PORT" --daemon >/dev/null 2>&1
  for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo "MOUNTFAIL $arm $n"; return 1; }

  # Sample peak committed and the realized window while the readers run.
  local peakfile="$W/peak.$arm.$n"; : > "$peakfile"
  ( while :; do curl -s "http://127.0.0.1:$PORT/metrics" \
      | grep -E '^lith_(prefetch_committed_bytes|readahead_window_blocks|open_handles) ' >> "$peakfile"
    sleep 0.1; done ) & local sampler=$!

  local t0 t1; t0=$(date +%s.%N)
  for i in $(seq 0 $((n-1))); do cat "$MP/${OBJS[$i]}" > /dev/null 2>&1 & done
  wait $(jobs -p | grep -v "$sampler" 2>/dev/null) 2>/dev/null || true
  t1=$(date +%s.%N)
  kill $sampler 2>/dev/null; wait $sampler 2>/dev/null

  local m; m=$(curl -s "http://127.0.0.1:$PORT/metrics")
  local peak win hnd ws
  peak=$(awk '/^lith_prefetch_committed_bytes /{if($2+0>m)m=$2+0} END{printf "%.0f",m}' "$peakfile")
  win=$(awk '/^lith_readahead_window_blocks /{if($2+0>m)m=$2+0} END{printf "%.0f",m}' "$peakfile")
  hnd=$(awk '/^lith_open_handles /{if($2+0>m)m=$2+0} END{printf "%.0f",m}' "$peakfile")
  ws=0; for i in $(seq 0 $((n-1))); do ws=$((ws + $(stat -c %s "$MP/${OBJS[$i]}" 2>/dev/null || echo 0))); done
  echo "RESULT|$arm|readers=$n|wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f",b-a}')|peakCommit=$peak|budget=$(sv "$m" lith_prefetch_budget_bytes)|win=$win|handles=$hnd|evicted=$(sv "$m" lith_prefetch_evicted_unread_total)|issued=$(sv "$m" lith_prefetch_issued_total)|s3=$(sv "$m" lith_s3_bytes_total)|ws=$ws"
  um
}

echo "===== #301 phase 1b $(date -u) : tier=$TIER rationed=$RATIONED unrationed=$UNRATIONED ====="
for n in 4 8 16; do
  cell rationed   "$n" "$RATIONED"
  cell unrationed "$n" "$UNRATIONED"
done
echo "===== done ====="
