#!/usr/bin/env bash
# Session-40 bootstrap: build lith from nfs-gateway branch; nfs-common + samtools
# + s5cmd (no ganesha — it auto-binds :2049). For the NFS gateway client-tuning
# grid, fork work, and multi-node campaign (#143/#144).
set -euxo pipefail
DEV=$(lsblk -bdn -o NAME,SIZE,TYPE | awk '$3=="disk"{print $2,$1}' | sort -rn | head -1 | awk '{print $2}')
ROOTDEV=$(findmnt -no SOURCE / | sed 's/[0-9]*$//;s#/dev/##')
if [ "$DEV" != "$ROOTDEV" ] && ! mountpoint -q /mnt/nvme; then
  sudo mkfs.ext4 -F "/dev/$DEV"; sudo mkdir -p /mnt/nvme; sudo mount "/dev/$DEV" /mnt/nvme; sudo chown "$(id -u):$(id -g)" /mnt/nvme
else
  sudo mkdir -p /mnt/nvme && sudo chown "$(id -u):$(id -g)" /mnt/nvme
fi
mkdir -p /mnt/nvme/work /mnt/nvme/cache
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq samtools fuse3 build-essential curl git nfs-common rpcbind >/dev/null
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.27.1.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.1.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
if ! command -v s5cmd >/dev/null; then cd /tmp && curl -sSL https://github.com/peak/s5cmd/releases/download/v2.2.2/s5cmd_2.2.2_Linux-arm64.tar.gz -o s5.tgz && tar xzf s5.tgz s5cmd && sudo mv s5cmd /usr/local/bin/; fi
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git checkout -q "${1:-nfs-gateway}" && go build -o bin/lith ./cmd/lith
echo "lith $(git rev-parse --short HEAD) on $(git rev-parse --abbrev-ref HEAD)"
echo BOOTSTRAP40_OK
