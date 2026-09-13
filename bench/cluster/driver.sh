#!/usr/bin/env bash
# bench/cluster/driver.sh — campaign driver (runs on the gateway box). Writes the
# per-run config.env, waits for N ready markers, releases each phase with a go
# marker, waits for N done markers, and prints each phase's exclusive window
# [min(start)-guard, max(end)+guard] for ledger extraction. #143 cluster campaign.
# Usage: driver.sh <run-id> <N> <bucket> <phases-csv> <config> [k=v ...extra env]
set -uo pipefail
export AWS_REGION=us-east-1
RUN=$1; N=$2; BUCKET=$3; PHASES_CSV=$4; CONFIG=$5; shift 5
B="s3://$BUCKET"; GUARD=60
# assemble config.env
{ echo "CONFIG=$CONFIG"; echo "PHASES=\"${PHASES_CSV//,/ }\""; for kv in "$@"; do echo "$kv"; done; } > /tmp/config.env
aws s3 cp /tmp/config.env "$B/barrier/$RUN/config.env" >/dev/null
echo "DRIVER run=$RUN config=$CONFIG phases=$PHASES_CSV — waiting for $N ready…"
while [ "$(aws s3 ls "$B/barrier/$RUN/ready/" 2>/dev/null | wc -l)" -lt "$N" ]; do sleep 3; done
echo "DRIVER all $N ready"
IFS=, read -ra PH <<< "$PHASES_CSV"
for ph in "${PH[@]}"; do
  printf 'go' | aws s3 cp - "$B/barrier/$RUN/go/$ph" >/dev/null
  while [ "$(aws s3 ls "$B/barrier/$RUN/done/$ph/" 2>/dev/null | wc -l)" -lt "$N" ]; do sleep 3; done
  # collect start/end across nodes
  tmp=$(mktemp -d); aws s3 cp "$B/barrier/$RUN/done/$ph/" "$tmp/" --recursive >/dev/null 2>&1
  smin=$(cat "$tmp"/* | sed -n 's/.*start=\([0-9.]*\).*/\1/p' | sort -n | head -1)
  emax=$(cat "$tmp"/* | sed -n 's/.*end=\([0-9.]*\).*/\1/p' | sort -n | tail -1)
  wall=$(awk -v a="$smin" -v b="$emax" 'BEGIN{printf "%.1f", b-a}')
  ws=$(date -u -d "@$(awk -v s="$smin" 'BEGIN{printf "%d", s-'$GUARD'}')" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || python3 -c "import datetime;print(datetime.datetime.utcfromtimestamp($smin-$GUARD).strftime('%Y-%m-%dT%H:%M:%SZ'))")
  we=$(date -u -d "@$(awk -v s="$emax" 'BEGIN{printf "%d", s+'$GUARD'}')" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || python3 -c "import datetime;print(datetime.datetime.utcfromtimestamp($emax+$GUARD).strftime('%Y-%m-%dT%H:%M:%SZ'))")
  rcs=$(cat "$tmp"/* | sed -n 's/.*rc=\([0-9]*\).*/\1/p' | sort -u | tr '\n' ',')
  echo "PHASE|$CONFIG|$ph|wall_s=$wall|rc=$rcs|window=$ws..$we"
  rm -rf "$tmp"
done
echo "DRIVER_DONE $CONFIG"
