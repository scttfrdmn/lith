#!/usr/bin/env bash
# Session-34 bootstrap: lith with frameless backing (feat/cargoship-frameless),
# cargoship v0.24.3, s5cmd, samtools/tabix, and a python venv for A2 (xarray/s3fs/zarr).
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
sudo apt-get install -y -qq samtools tabix fuse3 build-essential curl git python3-pip python3-venv unzip >/dev/null
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.23.4.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
if ! command -v s5cmd >/dev/null; then cd /tmp && curl -sSL https://github.com/peak/s5cmd/releases/download/v2.2.2/s5cmd_2.2.2_Linux-arm64.tar.gz -o s5.tgz && tar xzf s5.tgz s5cmd && sudo mv s5cmd /usr/local/bin/; fi
if ! command -v cargoship >/dev/null; then cd /tmp && curl -sSL "https://github.com/scttfrdmn/cargoship/releases/download/v0.24.3/cargoship_0.24.3_linux_arm64.tar.gz" -o cs.tgz && tar xzf cs.tgz cargoship && sudo mv cargoship /usr/local/bin/; fi
cargoship --version 2>&1 | head -1
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git fetch -q origin feat/cargoship-frameless
git checkout -q origin/main               && go build -o bin/lith-main ./cmd/lith
git checkout -q origin/feat/cargoship-frameless && go build -o bin/lith ./cmd/lith
echo BUILDS main=$(git rev-parse --short origin/main) frameless=$(git rev-parse --short HEAD)
python3 -m venv /mnt/nvme/venv && /mnt/nvme/venv/bin/pip -q install --upgrade pip >/dev/null
/mnt/nvme/venv/bin/pip -q install xarray s3fs zarr numcodecs >/dev/null
/mnt/nvme/venv/bin/python -c 'import xarray,s3fs,zarr;print("py ok")'
echo BOOTSTRAP34_OK
