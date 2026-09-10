#!/usr/bin/env bash
set -euxo pipefail
sudo mkdir -p /mnt/nvme && sudo chown "$(id -u):$(id -g)" /mnt/nvme && mkdir -p /mnt/nvme/work
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq samtools tabix fuse3 build-essential curl git python3-pip python3-venv >/dev/null
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.23.4.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git fetch -q origin feat/footer
git checkout -q feat/footer        && go build -o bin/lith-footer ./cmd/lith
git checkout -q origin/main        && go build -o bin/lith-main   ./cmd/lith
git checkout -q v0.2.2 2>/dev/null && go build -o bin/lith-v022   ./cmd/lith || { git checkout -q origin/main && go build -o bin/lith-v022 ./cmd/lith; }
git checkout -q feat/footer
python3 -m venv /mnt/nvme/venv && /mnt/nvme/venv/bin/pip -q install --upgrade pip >/dev/null && /mnt/nvme/venv/bin/pip -q install pyarrow >/dev/null
/mnt/nvme/venv/bin/python -c 'import pyarrow;print("pyarrow",pyarrow.__version__)'
echo BOOTSTRAP_SMALL_OK
