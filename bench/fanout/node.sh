#!/usr/bin/env bash
# node.sh MODE N RUN — one fan-out node. MODE=lith|copy. Fetched + run via the
# spawn array --command. Uses boto3 (preinstalled on the spore AMI) for all S3;
# reads its array index from its Name tag (<array>-<index>) via DescribeInstances.
set -u
MODE=$1; N=$2; RUN=$3
BKT=scttfrdmn-lith-bench
PFX="lith-bench/fanout/$RUN"
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq >/dev/null 2>&1
sudo apt-get install -y -qq samtools fuse3 >/dev/null 2>&1
grep -q '^user_allow_other' /etc/fuse.conf 2>/dev/null || echo 'user_allow_other' | sudo tee -a /etc/fuse.conf >/dev/null
sudo mkdir -p /mnt2/scratch && sudo chown "$(id -u)" /mnt2/scratch
mkdir -p /tmp/out
export REF_CACHE=/mnt2/scratch/refcache/%2s/%2s/%s; mkdir -p /mnt2/scratch/refcache

# Fetch shared artifacts (lith binary, prebuilt phase3/data index, key list).
python3 - "$BKT" <<'PY'
import boto3, sys, urllib.request
def imds(p):
    t=urllib.request.Request("http://169.254.169.254/latest/api/token",method="PUT",headers={"X-aws-ec2-metadata-token-ttl-seconds":"180"})
    tok=urllib.request.urlopen(t,timeout=3).read().decode()
    r=urllib.request.Request("http://169.254.169.254/latest/meta-data/"+p,headers={"X-aws-ec2-metadata-token":tok})
    return urllib.request.urlopen(r,timeout=3).read().decode()
b=sys.argv[1]; s3=boto3.client("s3",region_name=imds("placement/region"))
s3.download_file(b,"lith-bench/bin/lith","/tmp/lith")
s3.download_file(b,"lith-bench/idx/p3.idx","/tmp/p3.idx")
s3.download_file(b,"lith-bench/idx/fanout-64.txt","/tmp/fanout-64.txt")
PY
chmod +x /tmp/lith

# Index + shard (contiguous block of the 64 keys).
read INDEX SHARD < <(python3 - "$N" <<'PY'
import boto3, sys, math, urllib.request
def imds(p):
    t=urllib.request.Request("http://169.254.169.254/latest/api/token",method="PUT",headers={"X-aws-ec2-metadata-token-ttl-seconds":"180"})
    tok=urllib.request.urlopen(t,timeout=3).read().decode()
    r=urllib.request.Request("http://169.254.169.254/latest/meta-data/"+p,headers={"X-aws-ec2-metadata-token":tok})
    return urllib.request.urlopen(r,timeout=3).read().decode()
N=int(sys.argv[1]); iid=imds("instance-id")
ec2=boto3.client("ec2",region_name=imds("placement/region"))
tags={t["Key"]:t["Value"] for t in ec2.describe_instances(InstanceIds=[iid])["Reservations"][0]["Instances"][0].get("Tags",[])}
tok=tags.get("Name","").rsplit("-",1)[-1]; idx=int(tok) if tok.isdigit() else 0
keys=[l.split("\t")[0] for l in open("/tmp/fanout-64.txt") if l.strip() and not l.startswith("#")]
per=math.ceil(len(keys)/N)
print(idx, ",".join(keys[idx*per:(idx+1)*per]))
PY
)
IFS=',' read -ra KEYS <<< "$SHARD"
echo "NODE index=$INDEX mode=$MODE N=$N shard=${#KEYS[@]}"

T0=$(date +%s.%N); COPY_S=0
if [ "$MODE" = "lith" ]; then
	sudo pkill -9 -x lith 2>/dev/null || true; sudo fusermount3 -u /mnt/lith 2>/dev/null || true
	sudo mkdir -p /mnt/lith && sudo chown "$(id -u)" /mnt/lith
	sudo nohup /tmp/lith mount s3://1000genomes /mnt/lith --index-file /tmp/p3.idx --no-sign-request --disk-cache 0 --allow-other >/tmp/m.log 2>&1 &
	disown; sleep 6
	: > /tmp/paths
	for k in "${KEYS[@]}"; do echo "/mnt/lith/${k#phase3/data/}" >> /tmp/paths; done
	CT0=$(date +%s.%N)
	xargs -a /tmp/paths -P 8 -I{} bash -c 'samtools flagstat "{}" > "/tmp/out/$(basename {}).flagstat" 2>/dev/null'
	COMPUTE_S=$(awk -v a=$CT0 -v b=$(date +%s.%N) 'BEGIN{printf "%.1f",b-a}')
	sudo fusermount3 -u /mnt/lith 2>/dev/null || true
else
	DT0=$(date +%s.%N)
	python3 - "$SHARD" <<'PY'
import boto3, botocore, sys, os
from concurrent.futures import ThreadPoolExecutor
keys=[k for k in sys.argv[1].split(",") if k]
s3=boto3.client("s3",config=botocore.config.Config(signature_version=botocore.UNSIGNED))
def dl(k): s3.download_file("1000genomes",k,"/mnt2/scratch/"+os.path.basename(k))
with ThreadPoolExecutor(max_workers=8) as ex: list(ex.map(dl,keys))
PY
	COPY_S=$(awk -v a=$DT0 -v b=$(date +%s.%N) 'BEGIN{printf "%.1f",b-a}')
	: > /tmp/paths
	for k in "${KEYS[@]}"; do echo "/mnt2/scratch/$(basename $k)" >> /tmp/paths; done
	CT0=$(date +%s.%N)
	xargs -a /tmp/paths -P 8 -I{} bash -c 'samtools flagstat "{}" > "/tmp/out/$(basename {}).flagstat" 2>/dev/null'
	COMPUTE_S=$(awk -v a=$CT0 -v b=$(date +%s.%N) 'BEGIN{printf "%.1f",b-a}')
	rm -f /mnt2/scratch/*.cram
fi
T1=$(date +%s.%N)

# Upload each flagstat result + this node's timing.
python3 - "$BKT" "$PFX" "$INDEX" "$N" "$MODE" "$COPY_S" "$COMPUTE_S" "$T0" "$T1" <<'PY'
import boto3, sys, os, glob, json, urllib.request
def imds(p):
    t=urllib.request.Request("http://169.254.169.254/latest/api/token",method="PUT",headers={"X-aws-ec2-metadata-token-ttl-seconds":"180"})
    tok=urllib.request.urlopen(t,timeout=3).read().decode()
    r=urllib.request.Request("http://169.254.169.254/latest/meta-data/"+p,headers={"X-aws-ec2-metadata-token":tok})
    return urllib.request.urlopen(r,timeout=3).read().decode()
bkt,pfx,idx,N,mode,copy_s,cs,t0,t1=sys.argv[1:10]
s3=boto3.client("s3",region_name=imds("placement/region"))
for f in glob.glob("/tmp/out/*.flagstat"):
    s3.upload_file(f,bkt,f"{pfx}/{os.path.basename(f)}")
tm={"index":int(idx),"n":int(N),"mode":mode,"copy_s":float(copy_s),"compute_s":float(cs),
    "node_runtime_s":round(float(t1)-float(t0),1),"start":float(t0),"end":float(t1)}
s3.put_object(Bucket=bkt,Key=f"{pfx}/_timing/{idx}.json",Body=json.dumps(tm).encode())
print("UPLOADED",tm)
PY
echo "NODE_DONE index=$INDEX"
touch /tmp/SPAWN_COMPLETE   # signal --on-complete terminate
