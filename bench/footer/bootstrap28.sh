#!/usr/bin/env bash
set -euxo pipefail
DEV=$(lsblk -dn -o NAME,TYPE | awk '$2=="disk"{print $1}' | grep -v nvme0n1 | head -1 || true)
if [ -n "${DEV:-}" ] && ! mountpoint -q /mnt/nvme; then
  sudo mkfs.ext4 -F "/dev/$DEV"; sudo mkdir -p /mnt/nvme; sudo mount "/dev/$DEV" /mnt/nvme
  sudo chown "$(id -u):$(id -g)" /mnt/nvme
else
  sudo mkdir -p /mnt/nvme && sudo chown "$(id -u):$(id -g)" /mnt/nvme
fi
mkdir -p /mnt/nvme/work
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq samtools tabix fuse3 build-essential curl git python3-pip python3-venv >/dev/null
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.23.4.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git fetch -q origin int-base int-post
git checkout -q int-base && go build -o bin/lith-base ./cmd/lith
git checkout -q int-post && go build -o bin/lith-post ./cmd/lith
echo BUILDS; ls -la bin/
python3 -m venv /mnt/nvme/venv && /mnt/nvme/venv/bin/pip -q install --upgrade pip >/dev/null && /mnt/nvme/venv/bin/pip -q install pyarrow >/dev/null
/mnt/nvme/venv/bin/python -c 'import pyarrow;print("pyarrow",pyarrow.__version__)'
echo BOOTSTRAP28_OK
