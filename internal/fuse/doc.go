// SPDX-License-Identifier: Apache-2.0

// Package fuse implements the FUSE layer on the hanwen/go-fuse/v2 raw API.
// All mutating operations return EROFS. It is implemented in milestone M2;
// see the pinned Design issue, §4.4.
package fuse
