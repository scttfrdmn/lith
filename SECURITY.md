# Security Policy

## Supported versions

lith is pre-1.0. Security fixes are made against the latest tagged release and
`main`.

## Reporting a vulnerability

Please report suspected vulnerabilities privately, through
[GitHub private vulnerability reporting](https://github.com/scttfrdmn/lith/security/advisories/new)
(the repository's Security tab → "Report a vulnerability"). Do **not** open a
public issue for a security report.

Please include a description, reproduction steps, and the affected version or
commit. We will acknowledge receipt within a few business days and keep you
informed of progress.

## Scope notes

lith is read-only and writes nothing to the bucket. It does read S3 credentials
from the standard AWS credential chain (unless `--no-sign-request` is used) and
writes a local index and block cache to disk; the cache is not encrypted at rest
(out of scope for v0.x).

Operator responsibilities and trust boundaries:

- **`--index-file` must be trusted and immutable for the mount's lifetime.** lith
  memory-maps the index and serves the filesystem from it; a mutable or malicious
  index is a data-integrity boundary. A malformed image is rejected
  (`ErrCorruptIndex`) rather than trusted, but a semantically-crafted index is only
  as trustworthy as its source.
- **`--endpoint` must be `https` when requests are signed.** lith refuses a
  non-HTTPS endpoint unless `--no-sign-request` is set, so credentials are never
  sent in cleartext to an arbitrary host.
- **`--allow-other` exposes the whole mounted subtree to every local user** (files
  are world-readable `0444`/`0555` with no per-object access control), independent
  of the underlying S3 ACLs. Use it only on hosts where that is acceptable.
- **`--pprof` must never be exposed to an untrusted network.** The Go pprof handlers
  it serves are unauthenticated and reveal the process argv (`/debug/pprof/cmdline`)
  and an on-demand CPU/goroutine profiling denial-of-service (`/profile`, `/trace`);
  bind it to `127.0.0.1`. `--metrics` serves only Prometheus `/metrics` (no pprof).
