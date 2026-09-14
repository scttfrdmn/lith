# SPDX-License-Identifier: Apache-2.0
#
# lith ships as a distroless image built by goreleaser from the *same* static
# binary the release publishes as a tar.gz — this Dockerfile only copies that
# prebuilt binary in, it never compiles, so the image can never drift from the
# tagged artifact. lith is a pure-Go binary (CGO_ENABLED=0; the FUSE layer is
# github.com/hanwen/go-fuse, raw syscalls, no libfuse), so the static base with
# no libc is sufficient.
#
# The CI "docker build (no push)" check builds `lith` with `go build` first and
# then runs this same Dockerfile, so a PR that breaks the image fails the gate.
FROM gcr.io/distroless/static:nonroot

COPY lith /lith
COPY LICENSE /LICENSE

# `lith` as the entrypoint with no default command: `docker run … lith serve nfs`
# and `docker run … lith doctor …` both read naturally, and no subcommand runs
# by accident.
ENTRYPOINT ["/lith"]
