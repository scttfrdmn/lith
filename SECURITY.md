# Security Policy

## Supported versions

lith is pre-1.0. Security fixes are made against the latest tagged release and
`main`.

## Reporting a vulnerability

Please report suspected vulnerabilities privately. Do **not** open a public
issue for a security report.

- Preferred: [GitHub private vulnerability reporting](https://github.com/scttfrdmn/lith/security/advisories/new).
- Email: `security@example.com` *(placeholder — replace before v0.1.0)*.

Please include a description, reproduction steps, and the affected version or
commit. We will acknowledge receipt within a few business days and keep you
informed of progress.

## Scope notes

lith is read-only and writes nothing to the bucket. It does read S3 credentials
from the standard AWS credential chain (unless `--no-sign-request` is used) and
writes a local index and block cache to disk; the cache is not encrypted at rest
(out of scope for v0.x).
