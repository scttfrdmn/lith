#!/usr/bin/env bash
# Session-32 regression bootstrap: base(main) vs post(feat/sparse, tier2 off by default).
# No pyarrow — this box only runs the regression block (R4 base-vs-post + CRAM S1/8/R2).
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
sudo apt-get install -y -qq samtools fuse3 build-essential curl git >/dev/null
if ! command -v go >/dev/null; then cd /tmp && curl -sSLO https://go.dev/dl/go1.23.4.linux-arm64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz; fi
export PATH=$PATH:/usr/local/go/bin
if ! command -v s5cmd >/dev/null; then
  cd /tmp && curl -sSL https://github.com/peak/s5cmd/releases/download/v2.2.2/s5cmd_2.2.2_Linux-arm64.tar.gz -o s5.tgz && tar xzf s5.tgz s5cmd && sudo mv s5cmd /usr/local/bin/
fi
cd /mnt/nvme && rm -rf lith && git clone -q https://github.com/scttfrdmn/lith.git && cd lith && mkdir -p bin
git fetch -q origin feat/sparse
git checkout -q origin/main   && go build -o bin/lith-base ./cmd/lith
git checkout -q feat/sparse   && go build -o bin/lith-post ./cmd/lith
echo BUILDS $(git rev-parse --short HEAD); ls -la bin/
# Overture staged file list (R4)
export AWS_REGION=us-east-1
s5cmd ls "s3://scttfrdmn-lith-bench/lith-bench/overture-places/*" | awk '{print $4}' | sort > /mnt/nvme/work/ov8.txt
wc -l /mnt/nvme/work/ov8.txt
# Prebuilt phase3 index + key list (CRAM S1/8/R2)
s5cmd cp "s3://scttfrdmn-lith-bench/lith-bench/idx/p3.idx" /mnt/nvme/work/p3.idx
s5cmd cp "s3://scttfrdmn-lith-bench/lith-bench/idx/fanout-64.txt" /mnt/nvme/work/fanout-64.txt
wc -l /mnt/nvme/work/fanout-64.txt
echo BOOTSTRAP32_OK
