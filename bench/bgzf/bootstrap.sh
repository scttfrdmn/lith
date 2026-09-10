#!/usr/bin/env bash
set -euxo pipefail
# mount the local NVMe for scratch + build + ref cache
if ! mountpoint -q /mnt/nvme; then
  sudo mkfs.ext4 -F /dev/nvme0n1
  sudo mkdir -p /mnt/nvme
  sudo mount /dev/nvme0n1 /mnt/nvme
  sudo chown "$(id -u):$(id -g)" /mnt/nvme
fi
mkdir -p /mnt/nvme/work

export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq samtools tabix awscli fuse3 build-essential curl git >/dev/null

# Go (arm64)
if ! command -v go >/dev/null; then
  cd /tmp
  curl -sSLO https://go.dev/dl/go1.23.4.linux-arm64.tar.gz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz
fi
export PATH=$PATH:/usr/local/go/bin
go version

# clone + build both binaries
cd /mnt/nvme
rm -rf lith
git clone -q https://github.com/scttfrdmn/lith.git
cd lith
mkdir -p bin
git checkout -q feat/bgzf
go build -o bin/lith-bgzf ./cmd/lith
git checkout -q origin/main
go build -o bin/lith-base ./cmd/lith
git checkout -q feat/bgzf
echo "BUILDS:"; ls -la bin/
echo "BOOTSTRAP OK"
