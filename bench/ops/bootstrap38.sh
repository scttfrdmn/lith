#!/usr/bin/env bash
# Session-38 bootstrap: build lith (main) + lithnfsspike (nfs-spike branch);
# install fuse3, nfs-common, nfs-ganesha + VFS FSAL, samtools. For the NFS
# gateway prototype spike (#143/#144) on one host, loopback client.
set -euxo pipefail
DEV=$(lsblk -bdn -o NAME,SIZE,TYPE | awk '$3=="disk"{print $2,$1}' | sort -rn | head -1 | awk '{print $2}')
ROOTDEV=$(findmnt -no SOURCE / | sed 's/[0-9]*$//;s#/dev/##')
if [ "$DEV" != "$ROOTDEV" ] && ! mountpoint -q /mnt/nvme; then
  sudo mkfs.ext4 -F "/dev/$DEV"; sudo mkdir -p /mnt/nvme; sudo mount "/dev/$DEV" /mnt/nvme; sudo chown "$(id -u):$(id -g)" /mnt/nvme
else
  sudo mkdir -p /mnt/nvme && sudo chown "$(id -u):$(id -g)" /mnt/nvme
fi
mkdir -p /mnt/nvme/work
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq samtools fuse3 build-essential curl git nfs-common nfs-ganesha nfs-ganesha-vfs rpcbind >/dev/null 2>&1 || \
  sudo apt-get install -y -qq samtools fuse3 build-essential curl git nfs-common rpcbind >/dev/null 2>&1
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.27.1.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.1.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git checkout -q main && go build -o bin/lith ./cmd/lith
git checkout -q nfs-spike && go build -o bin/lithnfsspike ./cmd/lithnfsspike
echo "lith $(git rev-parse --short origin/main), spike $(git rev-parse --short HEAD)"
command -v ganesha.nfsd && ganesha.nfsd -v 2>&1 | head -1 || echo "GANESHA_NOT_INSTALLED"
echo BOOTSTRAP38_OK
