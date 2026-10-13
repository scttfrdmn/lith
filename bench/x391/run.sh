#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# #391 cross-region cell: can a per-handle rule (the evidence gate at some ratio) give the
# gate-off population fairness without the 1.96x ramp penalty?
#
# Runs on a box in us-west-2 reading s3://scttfrdmn-lith-bench (us-east-1), so the region pair
# is cross-region and lith's default turns the evidence gate OFF -- the regime #391 is about.
#
# Arms: def (gate off, the shipped cross-region default) and --readahead-evidence-ratio 2/4/16/64.
# Cells, each on a FRESH mount so every read is cold:
#   single : one reader, dd of a 2 GiB object        -> the ramp penalty (score: wall)
#   n4     : 4 concurrent readers, 512 MiB each      -> fairness at small N (score: max/min)
#   d16    : 16 concurrent readers, 512 MiB each     -> fairness at large N (score: max/min)
# Two reps, arm order rotated per rep so no arm always runs first in an hour.
set -uo pipefail

B=scttfrdmn-lith-bench
P=x391
W=/home/ubuntu/x391
MP=/home/ubuntu/mnt
OUT=$W/results.csv
mkdir -p "$W" "$MP"
cd "$W"
log() { echo "[x391 $(date -u +%H:%M:%S)] $*" | tee -a "$W/run.log"; }

log "bootstrap"
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fuse3 curl unzip bc >/dev/null
grep -q '^user_allow_other' /etc/fuse.conf || echo user_allow_other | sudo tee -a /etc/fuse.conf >/dev/null
if ! command -v aws >/dev/null 2>&1; then
	curl -fsSL -o /tmp/awscliv2.zip https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip
	(cd /tmp && unzip -q -o awscliv2.zip && sudo ./aws/install --update >/dev/null)
fi

log "lith v1.18.0, the signed release artifact"
curl -fsSL -o lith https://github.com/scttfrdmn/lith/releases/download/v1.18.0/lith_linux_arm64
echo "2771055a97ea14dba97bcfece76e8686b2806a8c3d8c7823b67430ebf0be4d16  lith" | sha256sum -c || { log "HASH MISMATCH"; exit 1; }
chmod +x lith
./lith version | tee -a "$W/run.log"

log "test objects (written once; random data so nothing compresses or dedups)"
if ! aws s3 ls "s3://$B/$P/big.bin" >/dev/null 2>&1; then
	head -c 2147483648 /dev/urandom >big.bin
	head -c 536870912 /dev/urandom >small.bin
	aws s3 cp big.bin "s3://$B/$P/big.bin" --only-show-errors
	for i in $(seq -w 1 16); do aws s3 cp small.bin "s3://$B/$P/s$i.bin" --only-show-errors; done
	rm -f big.bin small.bin
fi
aws s3 ls "s3://$B/$P/" | tee -a "$W/run.log"

log "round trip to the bucket's region (first byte of a 1-byte range, 5 samples)"
for _ in 1 2 3 4 5; do
	curl -s -o /dev/null -r 0-0 -w '%{time_starttransfer}\n' "https://$B.s3.us-east-1.amazonaws.com/$P/s01.bin" 2>/dev/null
done | tee -a "$W/rtt.txt"

flags_for() {
	case "$1" in
	def) echo "" ;;
	r*) echo "--readahead-evidence-ratio ${1#r}" ;;
	esac
}

mount_arm() {
	sync
	echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
	# shellcheck disable=SC2046
	./lith mount "s3://$B/$P" "$MP" --metrics 127.0.0.1:9101 --log-level warn $(flags_for "$1") >"$W/mount-$1.log" 2>&1 &
	for _ in $(seq 1 100); do
		mountpoint -q "$MP" && break
		sleep 0.2
	done
	mountpoint -q "$MP" || { log "MOUNT FAILED for $1"; cat "$W/mount-$1.log"; return 1; }
	# Read the arm back from the RUNNING mount rather than trusting the flag.
	sleep 0.5
	RATIO=$(curl -s 127.0.0.1:9101/metrics | awk '/^lith_readahead_evidence_ratio /{print $2}')
}

scrape() { curl -s 127.0.0.1:9101/metrics | awk -v m="$1" '$1==m{print $2}'; }

unmount_arm() {
	BYTES=$(scrape lith_s3_bytes_total)
	HELD=$(scrape lith_prefetch_pressure_held_total)
	fusermount3 -u "$MP" 2>/dev/null
	for _ in $(seq 1 50); do pgrep -x lith >/dev/null || break; sleep 0.2; done
	pkill -x lith 2>/dev/null
	sleep 0.5
}

# run_cell <rep> <cell> <arm> <file...>: concurrent dd of each file from a common start; one CSV
# row per reader with its completion time measured from that start.
run_cell() {
	local rep=$1 cell=$2 arm=$3
	shift 3
	mount_arm "$arm" || return
	local t0 pids=()
	t0=$(date +%s.%N)
	for f in "$@"; do
		(dd if="$MP/$f" of=/dev/null bs=1M status=none && echo "$f $(date +%s.%N)" >>"$W/ends.$$") &
		pids+=($!)
	done
	wait "${pids[@]}"
	unmount_arm
	while read -r f t1; do
		echo "$rep,$cell,$arm,$RATIO,$f,$(echo "$t1 - $t0" | bc -l),$BYTES,$HELD" >>"$OUT"
	done <"$W/ends.$$"
	rm -f "$W/ends.$$"
	log "rep $rep $cell $arm (ratio in force $RATIO) done"
}

echo "rep,cell,arm,ratio_in_force,reader,secs,s3_bytes,pressure_held" >"$OUT"
SMALL4=(s01.bin s02.bin s03.bin s04.bin)
SMALL16=(s01.bin s02.bin s03.bin s04.bin s05.bin s06.bin s07.bin s08.bin s09.bin s10.bin s11.bin s12.bin s13.bin s14.bin s15.bin s16.bin)
ARMS=(def r2 r4 r16 r64)
for rep in 1 2; do
	# rotate so the arm order differs between reps
	if [ "$rep" = 2 ]; then ARMS=(r64 r16 r4 r2 def); fi
	for arm in "${ARMS[@]}"; do run_cell "$rep" single "$arm" big.bin; done
	for arm in "${ARMS[@]}"; do run_cell "$rep" n4 "$arm" "${SMALL4[@]}"; done
	for arm in "${ARMS[@]}"; do run_cell "$rep" d16 "$arm" "${SMALL16[@]}"; done
done
log "ALL DONE"
touch "$W/DONE"
