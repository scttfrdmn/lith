// SPDX-License-Identifier: Apache-2.0

// Package version holds build-time identifying information for the lith
// binary. The values are overridden at link time via -ldflags; see the
// Makefile and .goreleaser.yaml.
package version

// These variables are set at build time with:
//
//	-ldflags "-X github.com/scttfrdmn/lith/internal/version.Version=... \
//	          -X github.com/scttfrdmn/lith/internal/version.Commit=...  \
//	          -X github.com/scttfrdmn/lith/internal/version.Date=..."
//
// They default to development-friendly values for `go run` and plain
// `go build` invocations.
var (
	// Version is the semantic version of the build (e.g. "v0.1.0").
	Version = "dev"
	// Commit is the git commit the build was produced from.
	Commit = "none"
	// Date is the RFC 3339 build timestamp.
	Date = "unknown"
)
