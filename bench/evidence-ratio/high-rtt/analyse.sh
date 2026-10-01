#!/usr/bin/env bash
# Reproduce the dispatch-burst finding from the reporting workload's published traces.
#
#   curl -L -o bigobj.tgz https://github.com/scttfrdmn/aws-gchp/raw/lith/input-layer/data/lith-gates/xregion-bigobj-traces.tgz
#   sha256  55764044a1be804ff3156869957b73b989ab09b4325919c87fd1e27b769f1004
#   tar xzf bigobj.tgz -C some/dir && analyse.sh some/dir
#
# Needs lith-pfreplay: on PATH, or pass PFR=/path/to/lith-pfreplay.
set -uo pipefail
D=${1:?usage: analyse.sh <extracted-trace-dir>}

# Fail loudly rather than printing an empty section: a diagnostic that silently produces
# nothing when its tool is missing is the failure mode this whole investigation kept finding.
PFR=${PFR:-$(command -v lith-pfreplay || true)}
[ -n "$PFR" ] || { echo "analyse.sh: lith-pfreplay not found. Build it and pass PFR=, e.g." >&2
	echo "  go build -o /tmp/pfr ./cmd/lith-pfreplay && PFR=/tmp/pfr $0 $D" >&2; exit 1; }
ls "$D"/xrbig/hco-dd-r0-5.csv >/dev/null 2>&1 || { echo "analyse.sh: $D does not look like the extracted archive (no xrbig/hco-dd-r0-5.csv)" >&2; exit 1; }

echo "=== dispatch LEAD per arm (how many reads before a demand read its block was prefetched) ==="
for f in xrbig/hco-dd-r0-5 xrlad/hco-dd-r40-1 xrbig/hco-dd-r4-2 xrlad/hco-dd-r1-1; do
	[ -f "$D/$f.csv" ] || continue
	printf '%-6s ' "$(basename "$f" | sed 's/hco-dd-//;s/-[0-9]*$//')"
	"$PFR" -lead "a/x=$D/$f.csv" 2>&1 | grep 'lead in reads' | sed 's/^ *//'
done

echo
echo "=== dispatch BATCH size per arm -- this is the finding ==="
python3 - "$D" <<'PY'
import csv, sys, os
D = sys.argv[1]
print(f"{'arm':6s} {'batches':>8} {'blocks':>7} {'maxBatch':>9} {'p99':>5}")
for f in ('xrbig/hco-dd-r0-5', 'xrlad/hco-dd-r40-1', 'xrbig/hco-dd-r4-2', 'xrlad/hco-dd-r1-1'):
    path = os.path.join(D, f + '.csv')
    if not os.path.exists(path):
        continue
    arm = f.split('-r')[1].split('-')[0]
    ds = []
    with open(path) as fh:
        for row in csv.DictReader(l for l in fh if not l.startswith('#')):
            d = int(row['dispatched'])
            if d > 0:
                ds.append(d)
    s = sorted(ds)
    print(f"r{arm:5s} {len(ds):>8} {sum(ds):>7} {max(ds):>9} {s[int(.99*(len(s)-1))]:>5}")
PY
