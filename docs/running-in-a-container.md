# Running in a container

`ghcr.io/scttfrdmn/lith` is a distroless image — just the static `lith` binary
on `gcr.io/distroless/static`, multi-arch (`linux/amd64` + `linux/arm64`), built
by the release workflow from the same binaries as the tarballs. `lith` is the
entrypoint with no default command, so every subcommand reads naturally:

```bash
docker run --rm ghcr.io/scttfrdmn/lith:latest version
docker run --rm ghcr.io/scttfrdmn/lith:latest doctor s3://1000genomes --no-sign-request
```

Tags: the exact version (`0.5.0`), the floating `major.minor` (`0.5`), and
`latest` (real releases only, never a prerelease). The image runs as a non-root
user (uid 65532); mounted volumes must be writable by it.

## What the image is for: the gateway

The image's real job is `lith serve nfs` — the [read-only NFSv3
gateway](serving-a-cluster.md). It is a **userspace server**: no privileges, no
`/dev/fuse`, no capabilities. It needs the listen port published, the index and
cache as volumes, and `/healthz` + `/readyz` wired to the orchestrator.

```bash
docker run -d --name lith-gw \
  -p 2049:2049 -p 9101:9101 \
  -v /data/index:/index:ro \
  -v /data/cache:/cache \
  ghcr.io/scttfrdmn/lith:latest \
  serve nfs s3://bucket/prefix \
    --index-file /index/dataset.idx \
    --disk-cache 100GB --disk-path /cache \
    --listen :2049 --metrics :9101
```

The default `--no-portmap` means clients mount with an explicit port and no
rpcbind — so only 2049 is published and no privileged port (111) is needed,
which is why the non-root image is enough. Mount it from a compute node exactly
as in [Serving a cluster](serving-a-cluster.md):

```bash
sudo mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,\
rsize=1048576,wsize=1048576,nconnect=4,actimeo=600 <gateway-host>:/ /mnt/data
```

`/healthz` is liveness (200 once the process is up); `/readyz` is readiness —
503 with a reason while the index loads, then 200 once the gateway is serving.
Wire both to your orchestrator so traffic only arrives after the index is up.

### Kubernetes

A minimal gateway Deployment with the health probes and volumes wired:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lith-gateway
spec:
  replicas: 1
  selector:
    matchLabels: { app: lith-gateway }
  template:
    metadata:
      labels: { app: lith-gateway }
    spec:
      containers:
        - name: lith
          image: ghcr.io/scttfrdmn/lith:0.5
          args:
            - serve
            - nfs
            - s3://bucket/prefix
            - --index-file=/index/dataset.idx
            - --disk-cache=100GB
            - --disk-path=/cache
            - --listen=:2049
            - --metrics=:9101
          ports:
            - { name: nfs, containerPort: 2049 }
            - { name: metrics, containerPort: 9101 }
          livenessProbe:
            httpGet: { path: /healthz, port: metrics }
          readinessProbe:
            httpGet: { path: /readyz, port: metrics }
          volumeMounts:
            - { name: index, mountPath: /index, readOnly: true }
            - { name: cache, mountPath: /cache }
      volumes:
        - name: index
          # a prebuilt index, e.g. from a ConfigMap, PVC, or an init container
          persistentVolumeClaim: { claimName: lith-index }
        - name: cache
          # size to the working set; emptyDir (ephemeral) or a fast PVC
          emptyDir: { sizeLimit: 100Gi }
```

Front it with a `Service` on 2049 and mount `<service>:/` from clients with the
recipe above. AWS credentials come from the pod's IAM role (IRSA/Pod Identity),
the same resolution `lith doctor` reports.

### Volumes: index and cache

- **Index** (`--index-file`): mount it read-only. Build it once (below) and hand
  it in via a PVC, a ConfigMap, or an init container. Or omit it and let the
  gateway list on startup (`--auto-index-limit`), or point at a published
  archive with `--cargoship`.
- **Cache** (`--disk-cache` + `--disk-path`): not optional for a gateway. Size
  it to the working set so a restart re-serves from local disk, not S3. Use fast
  ephemeral storage (local NVMe / `emptyDir`) — it is a cache, not state.

## Building an index in a container

`lith index build` and `lith doctor` are trivially containerizable — useful in
CI and as an **init container** that produces the index a gateway pod then reads:

```bash
docker run --rm -v /data/index:/index ghcr.io/scttfrdmn/lith:latest \
  index build s3://bucket/prefix --index-file /index/dataset.idx
```

`--cargoship` and `--keys` work the same way; the artifact lands on the mounted
volume for a sidecar or the gateway to pick up.

## The awkward case: FUSE mount inside a container

`lith mount` works in a container, but mounting is not a userspace-only
operation, so it needs more than the gateway does — and there is a caveat worth
knowing before you try:

```bash
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  ghcr.io/scttfrdmn/lith:latest \
  mount s3://1000genomes/phase3/data/HG00100/alignment /mnt \
    --no-sign-request
```

- `--device /dev/fuse` and `--cap-add SYS_ADMIN` (or a FUSE device plugin) are
  required — the kernel mount syscall needs them.
- **The mount is namespace-local.** It is visible only *inside that container*,
  not on the host and not in other containers, unless you explicitly share the
  mount (e.g. a bind-propagation `rshared` mount into the host). A user who
  skips this sees a mount that "works" in the container and is invisible
  everywhere else — that is expected, not a bug.

For sharing a dataset across containers or nodes, don't reach for per-container
FUSE mounts — run the **gateway** and mount it over NFS. That is what the image
is for.
