// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/scttfrdmn/lith/internal/version"
	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "lith %s\ncommit: %s\nbuilt:  %s\n",
				version.Version, version.Commit, version.Date)
			return err
		},
	}
}
