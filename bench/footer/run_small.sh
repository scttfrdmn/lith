#!/usr/bin/env bash
# c8g.large (small box, I/O-bound): R4 single (base/tier1+2/pyarrow) + R1 single (base/#115).
set -uo pipefail
W=/mnt/nvme/work; LOG=$W/small.log
exec > >(tee -a "$LOG") 2>&1
echo "===== small $(date -u) $(nproc)vcpu ====="
PY=/mnt/nvme/venv/bin/python; Q=/tmp/ft/pyquery.py
OVP=s3://overturemaps-us-west-2/release/2026-08-19.0/theme=places/type=place
OVREL=release/2026-08-19.0/theme=places/type=place; OVB=overturemaps-us-west-2
F0=part-00000-c7e47654-8483-5b8f-b183-7ba73334f7a5-c000.zstd.parquet
R1V=ALL.chr20.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
A=/mnt/nvme/lith/bench/assets
awk -F'[:-]' '{print $1"\t"($2-1)"\t"$3}' "$A/r1-regions-chr20-10kb.txt" > "$W/r1.bed"
MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/s.idx
sv(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
mb(){ awk -v b="$1" 'BEGIN{printf "%.0f",b/1048576}'; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }
mf(){ um; rm -f "$IDX"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$1" mount "$2" "$MP" --index-file "$IDX" --no-sign-request $4 --metrics :9701 --daemon
  for i in $(seq 1 100); do mountpoint -q "$MP" && break; sleep 0.5; done; mountpoint -q "$MP"||{ echo MFAIL "$2";return 1;}; }

r4(){ # label bin
  local c=()
  for r in 1 2 3; do mf "$2" "$OVP" x "--region us-west-2"||return 1
    local t0;t0=$(now);"$PY" "$Q" local "$MP/$F0" >/dev/null 2>&1;local t1;t1=$(now);c+=("$(el "$t0" "$t1")")
    [ "$r" = 1 ] && sb=$(mb "$(sv "$(curl -s http://127.0.0.1:9701/metrics)" lith_s3_bytes_total)"); um; done
  echo "SMALL|R4|$1|cold=${c[0]},${c[1]},${c[2]}|s3_MB=${sb:-NA}"; }
r4py(){ local c=(); for r in 1 2 3; do sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local t0;t0=$(now);"$PY" "$Q" s3 "$OVB/$OVREL/$F0" >/dev/null 2>&1;local t1;t1=$(now);c+=("$(el "$t0" "$t1")");done
  echo "SMALL|R4|pyarrow-native|cold=${c[0]},${c[1]},${c[2]}|s3_MB=NA"; }
r1(){ # label bin
  local pfx=s3://1000genomes/release/20130502 c=()
  for r in 1 2 3; do mf "$2" "$pfx" x ""||return 1
    local t0;t0=$(now);tabix -R "$W/r1.bed" "$MP/$R1V" >/dev/null 2>&1;local t1;t1=$(now);c+=("$(el "$t0" "$t1")")
    [ "$r" = 1 ] && sb=$(mb "$(sv "$(curl -s http://127.0.0.1:9701/metrics)" lith_s3_bytes_total)"); um; done
  echo "SMALL|R1|$1|cold=${c[0]},${c[1]},${c[2]}|s3_MB=${sb:-NA}"; }
r4 "base"    /mnt/nvme/lith/bin/lith-main
r4 "tier1+2" /mnt/nvme/lith/bin/lith-footer
r4py
r1 "base" /mnt/nvme/lith/bin/lith-v022
r1 "#115" /mnt/nvme/lith/bin/lith-main
echo "===== small DONE $(date -u) ====="
grep -E "^SMALL" "$LOG"
