// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"
)

// newRootCmd builds the top-level `lith` command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "lith",
		Short: "Read-only POSIX filesystem over an S3 bucket in its native layout",
		Long: "lith presents an existing S3 bucket as a read-only POSIX filesystem\n" +
			"in the bucket's native key layout. It writes nothing to the bucket and\n" +
			"serves metadata locally after an index build.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		newVersionCmd(),
		newIndexCmd(),
		newMountCmd(),
		newBenchCmd(),
	)
	return root
}
