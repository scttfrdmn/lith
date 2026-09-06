// SPDX-License-Identifier: Apache-2.0

// Command lith is a read-only, high-performance POSIX filesystem over an
// existing S3 bucket in its native key layout. See the pinned Design issue.
package main

import (
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
