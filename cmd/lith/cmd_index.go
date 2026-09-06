// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newIndexCmd is a placeholder in M0; the build/refresh/inspect subcommands
// are implemented in M1 (see the pinned Design issue, §5).
func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build, refresh, and inspect the namespace index (M1)",
	}
	notImpl := func(use, short string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(_ *cobra.Command, _ []string) error {
				return errors.New("index: not implemented in this build")
			},
		}
	}
	cmd.AddCommand(
		notImpl("build s3://bucket[/prefix]", "Build a new index"),
		notImpl("refresh s3://bucket[/prefix]", "Re-list and rewrite an index"),
		notImpl("inspect", "Print statistics for a built index"),
	)
	return cmd
}
