#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# devbox.sh — bootstrap a Linux/arm64 dev instance for lith FUSE work and
# benchmarking. Idempotent; safe to re-run. Intended for an Ubuntu 24.04 arm64
# box launched via spore.host `spawn`. Not used in production; this is tooling.
#
# Installs: fuse3, samtools, fio, awscli, mountpoint-s3 (arm64), and a Go
# toolchain matching the Mac. Mounts the local NVMe instance store at /mnt/nvme.

set -euo pipefail

GO_VERSION="${GO_VERSION:-1.27.1}"
NVME_MNT="${NVME_MNT:-/mnt/nvme}"

log() { echo "[devbox] $*"; }

log "apt packages"
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq fuse3 samtools fio curl ca-certificates unzip >/dev/null

log "aws cli v2 (arm64)"
if ! command -v aws >/dev/null 2>&1; then
	curl -fsSL -o /tmp/awscliv2.zip https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip
	(cd /tmp && unzip -q -o awscliv2.zip && sudo ./aws/install --update)
fi
aws --version

log "fuse: enable user_allow_other"
if ! grep -q '^user_allow_other' /etc/fuse.conf 2>/dev/null; then
	echo 'user_allow_other' | sudo tee -a /etc/fuse.conf >/dev/null
fi

log "mountpoint-s3 (arm64)"
if ! command -v mount-s3 >/dev/null 2>&1; then
	curl -fsSL -o /tmp/mount-s3.deb https://s3.amazonaws.com/mountpoint-s3-release/latest/arm64/mount-s3.deb
	sudo apt-get install -y -qq /tmp/mount-s3.deb >/dev/null
fi
mount-s3 --version

log "Go ${GO_VERSION}"
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" != "go${GO_VERSION}" ]; then
	curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz"
	sudo rm -rf /usr/local/go
	sudo tar -C /usr/local -xzf /tmp/go.tgz
fi
if ! grep -q '/usr/local/go/bin' "$HOME/.profile" 2>/dev/null; then
	echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >>"$HOME/.profile"
fi
export PATH=$PATH:/usr/local/go/bin
go version

log "mount local NVMe instance store at ${NVME_MNT}"
mount_nvme() {
	if mountpoint -q "$NVME_MNT"; then
		log "already mounted"
		return 0
	fi
	# The instance store shows up as an NVMe device whose model is
	# "Amazon EC2 NVMe Instance Storage" (root EBS is "Amazon Elastic Block Store").
	local dev=""
	while read -r name model; do
		case "$model" in
		*Instance*Storage*) dev="/dev/$name" ;;
		esac
	done < <(lsblk -dno NAME,MODEL)
	if [ -z "$dev" ]; then
		log "no instance-store NVMe found; skipping (using root volume)"
		return 0
	fi
	log "formatting $dev as ext4"
	sudo mkfs.ext4 -F -q "$dev"
	sudo mkdir -p "$NVME_MNT"
	sudo mount "$dev" "$NVME_MNT"
	sudo chown "$(id -u):$(id -g)" "$NVME_MNT"
	df -h "$NVME_MNT"
}
mount_nvme

# bgrun: run a long benchmark detached to a file so its progress survives the
# SSH connection and can be polled. `spawn connect` block-buffers a remote
# command's stdout (spore-host/spawn#582), so a multi-minute run shows nothing
# until it exits — and if the box hits its TTL first the output is lost. Instead:
#   spawn connect <box> --user ubuntu -- bgrun ./bench.sh      # returns at once
#   spawn connect <box> --user ubuntu -- tail -n +1 /tmp/bgrun.out   # poll
# Application-benchmark tools (session 13). Opt-in — set LITH_BENCH_APPS=1 — so
# the base dev box stays lean. Pinned versions; arm64 wheels/apt.
if [ "${LITH_BENCH_APPS:-0}" = "1" ]; then
	log "app-bench tools: apt (samtools, tabix, fastp)"
	sudo apt-get install -y -qq tabix fastp python3-venv python3-pip >/dev/null 2>&1 || true
	samtools --version | head -1; tabix --version 2>&1 | head -1; fastp --version 2>&1 | head -1
	log "app-bench tools: python venv /opt/lithapps (pinned)"
	python3 -m venv /opt/lithapps
	/opt/lithapps/bin/pip -q install --upgrade pip >/dev/null 2>&1
	/opt/lithapps/bin/pip -q install \
		numpy==1.26.4 \
		'xarray==2024.6.0' \
		'zarr==2.18.2' \
		's3fs==2024.6.1' \
		'h5py==3.11.0' \
		'pyarrow==17.0.0' \
		'webdataset==0.2.100' \
		'torch==2.4.1' --extra-index-url https://download.pytorch.org/whl/cpu >/dev/null 2>&1 \
		&& echo "lithapps venv ready" || echo "WARN: some app wheels failed; see box"
fi

log "install bgrun helper (detached-to-file runner for long benchmarks)"
sudo tee /usr/local/bin/bgrun >/dev/null <<'BGRUN'
#!/usr/bin/env bash
# bgrun [-o OUT] CMD...  — run CMD detached, append stdout+stderr to OUT
# (default /tmp/bgrun.out), print the output path, and return immediately.
out=/tmp/bgrun.out
if [ "$1" = "-o" ]; then out="$2"; shift 2; fi
: >"$out"
nohup "$@" >>"$out" 2>&1 &
echo "bgrun pid=$! out=$out"
BGRUN
sudo chmod +x /usr/local/bin/bgrun

log "done"
