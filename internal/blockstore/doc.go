// SPDX-License-Identifier: Apache-2.0

// Package blockstore implements the read-only block cache over range GETs
// (memory and NVMe disk tiers, range coalescing, singleflight). It is
// implemented in milestone M2; see the pinned Design issue, §4.2.
package blockstore
