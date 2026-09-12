#!/usr/bin/env bash
# Session-35 bootstrap: lith v0.3.1 (main) + lith-main, cargoship v0.24.4,
# s5cmd, mountpoint-s3, samtools/tabix, python (pyarrow/xarray/s3fs/zarr).
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
if ! command -v cargoship >/dev/null; then cd /tmp && curl -sSL "https://github.com/scttfrdmn/cargoship/releases/download/v0.24.4/cargoship_0.24.4_linux_arm64.tar.gz" -o cs.tgz && tar xzf cs.tgz cargoship && sudo mv cargoship /usr/local/bin/; fi
# mountpoint-s3 (arm64 deb)
if ! command -v mount-s3 >/dev/null; then cd /tmp && curl -sSL "https://s3.amazonaws.com/mountpoint-s3-release/latest/arm64/mount-s3.deb" -o mount-s3.deb && sudo apt-get install -y -qq ./mount-s3.deb >/dev/null 2>&1 || true; fi
cargoship --version 2>&1 | head -1; mount-s3 --version 2>&1 | head -1 || echo "mount-s3 not installed"
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git checkout -q main && go build -o bin/lith ./cmd/lith && cp bin/lith bin/lith-main
echo BUILD $(git rev-parse --short HEAD)
python3 -m venv /mnt/nvme/venv && /mnt/nvme/venv/bin/pip -q install --upgrade pip >/dev/null
/mnt/nvme/venv/bin/pip -q install pyarrow xarray s3fs zarr numcodecs >/dev/null
/mnt/nvme/venv/bin/python -c 'import pyarrow,xarray,s3fs,zarr;print("py ok")'
echo BOOTSTRAP35_OK
