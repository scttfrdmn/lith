#!/usr/bin/env bash
# session-26 re-measure driver: base vs reworked-bgzf on R1/R2 (+single-region + S1).
# GRCh37 1000genomes public objects (--no-sign-request), us-east-1.
set -uo pipefail
W=/mnt/nvme/work
LOG=$W/results.log
exec > >(tee -a "$LOG") 2>&1
echo "===== $(date -u) ====="

BUCKET=s3://1000genomes
R1_PREFIX=$BUCKET/release/20130502
R1_VCF=ALL.chr20.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
R2_PREFIX=$BUCKET/phase3/data/HG00100/alignment
R2_CRAM=HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
S1_PREFIX=$BUCKET/phase3/data/HG00096/alignment
S1_CRAM=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram

export REF_PATH="http://www.ebi.ac.uk/ena/cram/md5/%s"
export REF_CACHE="$W/refcache/%2s/%2s/%s"
mkdir -p "$W/refcache"

# region files -> BED (chr20, GRCh37, no 'chr' prefix)
R1BED=$W/r1.bed; R2BED=$W/r2.bed
awk -F'[:-]' '{print $1"\t"($2-1)"\t"$3}' /mnt/nvme/lith/bench/assets/r1-regions-chr20-10kb.txt > "$R1BED"
awk -F'[:-]' '{print $1"\t"($2-1)"\t"$3}' /mnt/nvme/lith/bench/assets/r2-regions-1Mb.txt   > "$R2BED"

scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1" | tail -1; }
mvalf(){ awk -v k="$2" '$0 ~ "^"k"{"{s+=$2} END{print s+0}' <<<"$1"; } # summed over labels

MP=/mnt/nvme/mp
mkdir -p "$MP"
IDX=$W/idx.lithidx

umount_mp(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }

# mount_fresh BIN PREFIX PORT  -> new lith process (empty block cache) + drop page cache
mount_fresh(){
  local bin=$1 prefix=$2 port=$3
  umount_mp
  rm -f "$IDX"
  sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$bin" mount "$prefix" "$MP" --index-file "$IDX" --no-sign-request --metrics ":$port" --daemon
  for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo "MOUNT FAILED $bin $prefix"; return 1; }
}

now(){ date +%s.%N; }
elapsed(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f", b-a}'; }

# query KIND -> runs the tool against the mount
query(){
  case "$1" in
    R1) tabix -R "$R1BED" "$MP/$R1_VCF" >/dev/null 2>&1 ;;
    R2) samtools view -c -L "$R2BED" "$MP/$R2_CRAM" >/dev/null 2>&1 ;;
    R2single) samtools view -c "$MP/$R2_CRAM" 20:30000000-31000000 >/dev/null 2>&1 ;;
    S1) samtools flagstat "$MP/$S1_CRAM" >/dev/null 2>&1 ;;
  esac
}

# measure LABEL BIN KIND PREFIX PORT  -> 3 cold + 1 warm + metrics (bytes/ranges/unread on cold rep1)
measure(){
  local label=$1 bin=$2 kind=$3 prefix=$4 port=$5
  echo "---- $label : $kind ----"
  # untimed ref-cache warmup (populates REF_CACHE on disk; not part of any timing)
  mount_fresh "$bin" "$prefix" "$port" || return 1
  query "$kind"; umount_mp
  local colds=() warm="" m0 m1 s3b ranges unread idxb
  for rep in 1 2 3; do
    mount_fresh "$bin" "$prefix" "$port" || return 1
    if [ "$rep" = 1 ]; then m0=$(scrape "$port"); fi
    local t0; t0=$(now); query "$kind"; local t1; t1=$(now)
    colds+=("$(elapsed "$t0" "$t1")")
    if [ "$rep" = 1 ]; then
      m1=$(scrape "$port")
      s3b=$(mval "$m1" lith_s3_bytes_total)
      local s3b0; s3b0=$(mval "$m0" lith_s3_bytes_total)
      s3b=$(awk -v a="${s3b0:-0}" -v b="${s3b:-0}" 'BEGIN{printf "%.0f", b-a}')
      ranges=$(mvalf "$m1" lith_format_plan_ranges_total)
      unread=$(mval "$m1" lith_prefetch_evicted_unread_total)
      idxb=$(mval "$m1" lith_format_index_prefetch_bytes_total)
      local tw0; tw0=$(now); query "$kind"; local tw1; tw1=$(now)
      warm=$(elapsed "$tw0" "$tw1")
    fi
    umount_mp
  done
  echo "RESULT|$label|$kind|cold=${colds[0]},${colds[1]},${colds[2]}|warm=$warm|s3_bytes=${s3b}|plan_ranges=${ranges:-0}|unread_evicted=${unread:-0}|idx_pf_bytes=${idxb:-0}"
}

BB=/mnt/nvme/lith/bin/lith-base
BG=/mnt/nvme/lith/bin/lith-bgzf

measure "base"  "$BB" R1 "$R1_PREFIX" 9101
measure "bgzf"  "$BG" R1 "$R1_PREFIX" 9102
measure "base"  "$BB" R2 "$R2_PREFIX" 9103
measure "bgzf"  "$BG" R2 "$R2_PREFIX" 9104
measure "base"  "$BB" R2single "$R2_PREFIX" 9105
measure "bgzf"  "$BG" R2single "$R2_PREFIX" 9106
measure "base"  "$BB" S1 "$S1_PREFIX" 9107
measure "bgzf"  "$BG" S1 "$S1_PREFIX" 9108

echo "===== DONE $(date -u) ====="
grep '^RESULT|' "$LOG"
