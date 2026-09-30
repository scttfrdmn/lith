#!/usr/bin/env bash
# Price the request-side cost of --readahead-evidence-ratio on a WHOLE-FILE sequential
# read -- the case that PAYS for the flag, since it wants every byte and can only lose.
#
# Both arms are lith, so lith's own counters are the right instrument; the CloudTrail
# ledger exists for comparing lith against a different reader, not lith against itself.
#
# --nic-gbps 50 on a box whose real baseline is 7.5 over-sizes the window relative to the
# pipe (#239). That is ILLEGITIMATE for a partial-read byte measurement and FINE here: a
# whole-file reader wants every byte regardless of window size, so the bytes fetched are
# identical by construction and the only thing the declared NIC changes is the ramp length
# and request pattern -- which is exactly what is being measured. It is also the only way
# to reach max_readahead=223, the regime the reporting workload measured at.
set -uo pipefail
MP=/mnt/mp; W=/tmp/w2; IDX=$W/idx; PORT=9711
mkdir -p "$W"
P=s3://1000genomes/release/20130502
REG="--region us-east-1 --no-sign-request"
C20=ALL.chr20.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
C1=ALL.chr1.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
REPS=5   # rep 1 is discarded as warm-up; the trend in v1 was monotone within every arm

# Labelled metrics must be summed across label sets, not matched exactly: v1 read
# lith_s3_requests_total as 0 for every arm because the series is
# lith_s3_requests_total{op="get",status="ok"}.
sv(){ awk -v k="$2" 'index($1,k)==1 {s+=$NF} END{printf "%.0f", s+0}' <<<"$1"; }
now(){ date +%s.%N; }
el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; sleep 1; }

run(){ local lbl=$1 f=$2 ratio=$3 nic=$4
  local walls=() reqs=() bytes=() iss=() mra=""
  for r in $(seq 1 $REPS); do
    um; rm -f "$IDX"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local extra=""; [ "$ratio" != "0" ] && extra="--readahead-evidence-ratio $ratio"
    local tf=""; [ "$r" = 1 ] && tf="--pf-trace $W/$lbl-r$ratio-n$nic.csv"
    /tmp/lith mount "$P" "$MP" --index-file "$IDX" $REG --nic-gbps "$nic" \
      --metrics :$PORT --daemon $extra $tf >/dev/null 2>&1
    for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done
    mountpoint -q "$MP" || { echo "$lbl MOUNTFAIL"; return 1; }
    local t0 t1; t0=$(now); cat "$MP/$f" > /dev/null 2>&1; t1=$(now)
    local m; m=$(curl -s "http://127.0.0.1:$PORT/metrics")
    if [ "$r" -gt 1 ]; then
      walls+=("$(el "$t0" "$t1")")
      reqs+=("$(sv "$m" lith_s3_requests_total)")
      bytes+=("$(sv "$m" lith_s3_bytes_total)")
      iss+=("$(sv "$m" lith_prefetch_issued_total)")
    fi
    [ -z "$mra" ] && [ -f "$W/$lbl-r$ratio-n$nic.csv" ] && \
      mra=$(grep -m1 '^#' "$W/$lbl-r$ratio-n$nic.csv" | grep -oE 'max_readahead=[0-9]+' | cut -d= -f2)
    um
  done
  local mw mr mb mi
  mw=$(printf '%s\n' "${walls[@]}" | awk '{s+=$1;n++} END{printf "%.2f",s/n}')
  mr=$(printf '%s\n' "${reqs[@]}"  | awk '{s+=$1;n++} END{printf "%.0f",s/n}')
  mb=$(printf '%s\n' "${bytes[@]}" | awk '{s+=$1;n++} END{printf "%.0f",s/n}')
  mi=$(printf '%s\n' "${iss[@]}"   | awk '{s+=$1;n++} END{printf "%.0f",s/n}')
  echo "RESULT|$lbl|nic=$nic|mra=${mra:-NA}|ratio=$ratio|wall_mean=$mw|wall=$(IFS=,;echo "${walls[*]}")|req=$mr|bytes=$mb|issued=$mi"
}

echo "===== evidence-ratio request-cost gate v2 $(date -u) ====="
echo "box: $(nproc)vcpu $(uname -m); reps=$REPS (rep 1 discarded as warm-up)"
for nic in 50 7.5 3; do
  run "chr20" "$C20" 0 "$nic"; run "chr20" "$C20" 4 "$nic"
  run "chr1"  "$C1"  0 "$nic"; run "chr1"  "$C1"  4 "$nic"
done
echo "===== done ====="
