#!/usr/bin/env bash
# R2 (region-args, index-driven selective) + R2single + S1. Appends to results.log.
set -uo pipefail
W=/mnt/nvme/work
LOG=$W/results.log
exec > >(tee -a "$LOG") 2>&1
echo "===== run2 $(date -u) ====="

BUCKET=s3://1000genomes
R2_PREFIX=$BUCKET/phase3/data/HG00100/alignment
R2_CRAM=HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
S1_PREFIX=$BUCKET/phase3/data/HG00096/alignment
S1_CRAM=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram

export REF_PATH="http://www.ebi.ac.uk/ena/cram/md5/%s"
export REF_CACHE="$W/refcache/%2s/%2s/%s"
mkdir -p "$W/refcache"

# 1000×1Mb regions as arguments (index-driven, chunk-cache dedups overlap → ~0.8 GB)
R2ARGS=$(tr '\n' ' ' < /mnt/nvme/lith/bench/assets/r2-regions-1Mb.txt)

scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1" | tail -1; }
mvalf(){ awk -v k="$2" '$0 ~ "^"k"{"{s+=$2} END{print s+0}' <<<"$1"; }

MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/idx.lithidx
umount_mp(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
mount_fresh(){
  local bin=$1 prefix=$2 port=$3
  umount_mp; rm -f "$IDX"
  sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$bin" mount "$prefix" "$MP" --index-file "$IDX" --no-sign-request --metrics ":$port" --daemon
  for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo "MOUNT FAILED $bin $prefix"; return 1; }
}
now(){ date +%s.%N; }
elapsed(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f", b-a}'; }
query(){
  case "$1" in
    R2)       samtools view -c "$MP/$R2_CRAM" $R2ARGS >/dev/null 2>&1 ;;
    R2single) samtools view -c "$MP/$R2_CRAM" 20:30000000-31000000 >/dev/null 2>&1 ;;
    S1)       samtools flagstat "$MP/$S1_CRAM" >/dev/null 2>&1 ;;
  esac
}
measure(){
  local label=$1 bin=$2 kind=$3 prefix=$4 port=$5
  echo "---- $label : $kind ----"
  mount_fresh "$bin" "$prefix" "$port" || return 1
  query "$kind"; umount_mp   # untimed ref-cache warmup
  local colds=() warm="" m0 m1 s3b s3b0 ranges unread idxb
  for rep in 1 2 3; do
    mount_fresh "$bin" "$prefix" "$port" || return 1
    [ "$rep" = 1 ] && m0=$(scrape "$port")
    local t0; t0=$(now); query "$kind"; local t1; t1=$(now)
    colds+=("$(elapsed "$t0" "$t1")")
    if [ "$rep" = 1 ]; then
      m1=$(scrape "$port")
      s3b0=$(mval "$m0" lith_s3_bytes_total); s3b=$(mval "$m1" lith_s3_bytes_total)
      s3b=$(awk -v a="${s3b0:-0}" -v b="${s3b:-0}" 'BEGIN{printf "%.0f", b-a}')
      ranges=$(mvalf "$m1" lith_format_plan_ranges_total)
      unread=$(mval "$m1" lith_prefetch_evicted_unread_total)
      idxb=$(mval "$m1" lith_format_index_prefetch_bytes_total)
      local tw0; tw0=$(now); query "$kind"; local tw1; tw1=$(now); warm=$(elapsed "$tw0" "$tw1")
    fi
    umount_mp
  done
  echo "RESULT2|$label|$kind|cold=${colds[0]},${colds[1]},${colds[2]}|warm=$warm|s3_bytes=${s3b}|plan_ranges=${ranges:-0}|unread_evicted=${unread:-0}|idx_pf_bytes=${idxb:-0}"
}

BB=/mnt/nvme/lith/bin/lith-base
BG=/mnt/nvme/lith/bin/lith-bgzf
measure "base" "$BB" R2       "$R2_PREFIX" 9203
measure "bgzf" "$BG" R2       "$R2_PREFIX" 9204
measure "base" "$BB" R2single "$R2_PREFIX" 9205
measure "bgzf" "$BG" R2single "$R2_PREFIX" 9206
measure "base" "$BB" S1       "$S1_PREFIX" 9207
measure "bgzf" "$BG" S1       "$S1_PREFIX" 9208
echo "===== run2 DONE $(date -u) ====="
grep -E '^RESULT2?\|' "$LOG"
