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

log "done"
