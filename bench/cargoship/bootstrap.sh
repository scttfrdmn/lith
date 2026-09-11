#!/usr/bin/env bash
# Session-33 (#94) bootstrap: build lith with the CargoShip backing (both PRs on
# feat/cargoship-backing), install cargoship v0.24.2, samtools, and a python venv
# for the A1/A2/A3 comparators.
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
# cargoship v0.24.2 (arm64 release binary)
if ! command -v cargoship >/dev/null; then
  cd /tmp && curl -sSL "https://github.com/scttfrdmn/cargoship/releases/download/v0.24.2/cargoship_Linux_arm64.tar.gz" -o cs.tgz && tar xzf cs.tgz cargoship 2>/dev/null && sudo mv cargoship /usr/local/bin/ || true
fi
cargoship --version 2>&1 | head -1 || echo "cargoship not installed via release; will try go install"
# lith with the backing (both PRs)
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git fetch -q origin feat/cargoship-backing feat/cargoship-index
git checkout -q origin/main            && go build -o bin/lith-main ./cmd/lith
git checkout -q feat/cargoship-backing && go build -o bin/lith ./cmd/lith
echo BUILDS main=$(git rev-parse --short origin/main) backing=$(git rev-parse --short HEAD); ls -la bin/
python3 -m venv /mnt/nvme/venv && /mnt/nvme/venv/bin/pip -q install --upgrade pip >/dev/null
/mnt/nvme/venv/bin/pip -q install pyarrow xarray s3fs zarr numcodecs >/dev/null
/mnt/nvme/venv/bin/python -c 'import xarray,s3fs,zarr,pyarrow;print("py deps ok")'
echo BOOTSTRAP33_OK
