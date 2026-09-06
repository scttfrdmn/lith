// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func newMountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount s3://bucket[/prefix] /mnt/point",
		Short: "Mount an S3 bucket as a read-only POSIX filesystem (M2)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("mount: not implemented in this build")
		},
	}
}
