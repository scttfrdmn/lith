#!/usr/bin/env bash
# bgzf owed numbers under the amended rule: R1/R2 8-concurrent cold (base vs #115),
# S1 no-regression. 1000genomes, us-east-1 (same region). Appends BGZF lines.
set -uo pipefail
W=/mnt/nvme/work; LOG=$W/bgzf.log
exec > >(tee -a "$LOG") 2>&1
echo "===== bgzf $(date -u) ====="
export REF_PATH="http://www.ebi.ac.uk/ena/cram/md5/%s" REF_CACHE="$W/refcache/%2s/%2s/%s"
mkdir -p "$W/refcache"
R1V=ALL.chr20.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
R2C=HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
S1C=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
A=/mnt/nvme/lith/bench/assets
awk -F'[:-]' '{print $1"\t"($2-1)"\t"$3}' "$A/r1-regions-chr20-10kb.txt" > "$W/r1.bed"
R2ARGS=$(tr '\n' ' ' < "$A/r2-regions-1Mb.txt")

MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/bg.idx
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
umount_mp(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
mount_fresh(){ # bin prefix port
  umount_mp; rm -f "$IDX"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$1" mount "$2" "$MP" --index-file "$IDX" --no-sign-request --metrics ":$3" --daemon
  for i in $(seq 1 80); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo MOUNT_FAIL "$2"; return 1; }
}
mb(){ awk -v b="$1" 'BEGIN{printf "%.0f",b/1048576}'; }

R1conc(){ # label bin
  local pfx=s3://1000genomes/release/20130502
  mount_fresh "$2" "$pfx" 9601 || return 1
  for i in $(seq 1 8); do tabix -R "$W/r1.bed" "$MP/$R1V" >/dev/null 2>&1; done # ref-cache warm (n/a for vcf)
  umount_mp
  for rep in 1 2; do
    mount_fresh "$2" "$pfx" 9601 || return 1
    local t0; t0=$(now); pids=()
    for i in $(seq 1 8); do tabix -R "$W/r1.bed" "$MP/$R1V" >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); m=$(scrape 9601)
    echo "BGZF|R1conc8|$1|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")"
    umount_mp
  done
}
R2conc(){ # label bin
  local pfx=s3://1000genomes/phase3/data/HG00100/alignment
  mount_fresh "$2" "$pfx" 9602 || return 1
  samtools view -c "$MP/$R2C" $R2ARGS >/dev/null 2>&1 # ref warm
  umount_mp
  for rep in 1 2; do
    mount_fresh "$2" "$pfx" 9602 || return 1
    local t0; t0=$(now); pids=()
    for i in $(seq 1 8); do samtools view -c "$MP/$R2C" $R2ARGS >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); m=$(scrape 9602)
    echo "BGZF|R2conc8|$1|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")"
    umount_mp
  done
}
S1(){ # label bin
  local pfx=s3://1000genomes/phase3/data/HG00096/alignment
  mount_fresh "$2" "$pfx" 9603 || return 1
  samtools flagstat "$MP/$S1C" >/dev/null 2>&1; umount_mp # ref warm
  mount_fresh "$2" "$pfx" 9603 || return 1
  local t0; t0=$(now); samtools flagstat "$MP/$S1C" >/dev/null 2>&1; local t1; t1=$(now); m=$(scrape 9603)
  echo "BGZF|S1|$1|cold=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")"
  umount_mp
}
BV=/mnt/nvme/lith/bin/lith-v022
BM=/mnt/nvme/lith/bin/lith-main
R1conc "base"  "$BV"
R1conc "#115"  "$BM"
R2conc "base"  "$BV"
R2conc "#115"  "$BM"
S1 "base" "$BV"
S1 "#115" "$BM"
echo "===== bgzf DONE $(date -u) ====="
grep -E "^BGZF" "$LOG"
