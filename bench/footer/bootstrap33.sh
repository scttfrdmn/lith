#!/usr/bin/env bash
# Session-32 CRAM regression bootstrap: base = pre-#122 (ddba4d8), post = c337973.
# Rebuilds a v4 index over the 8 test CRAMs + their .crai siblings (the shipped
# p3.idx is v2, which the v4 build rejects — a ~1s --keys rebuild, not a blocker).
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
git checkout -q ddba4d8 && go build -o bin/lith-base ./cmd/lith   # pre-#122
git checkout -q c337973 && go build -o bin/lith-post ./cmd/lith   # main (+#122)
echo BUILDS base=ddba4d8 post=$(git rev-parse --short HEAD)
export AWS_REGION=us-east-1
# fanout key list (CRAMs)
s5cmd cp "s3://scttfrdmn-lith-bench/lith-bench/idx/fanout-64.txt" /mnt/nvme/work/fanout-64.txt
# v4 index over the 8 test CRAMs + their .crai siblings, via --keys (bgzf needs the crai in the index)
grep -v '^#' /mnt/nvme/work/fanout-64.txt | head -8 | awk -F'\t' '{print $1"\t"$2; print $1".crai"}' > /mnt/nvme/work/keys16.txt
wc -l /mnt/nvme/work/keys16.txt
t0=$(date +%s.%N)
bin/lith-post index build s3://1000genomes --keys /mnt/nvme/work/keys16.txt --no-sign-request --index-file /mnt/nvme/work/p3v4.idx
t1=$(date +%s.%N); awk -v a=$t0 -v b=$t1 'BEGIN{printf "index rebuild: %.1fs\n",b-a}'
bin/lith-post index inspect /mnt/nvme/work/p3v4.idx 2>&1 | head -8 || true
echo BOOTSTRAP33_OK
