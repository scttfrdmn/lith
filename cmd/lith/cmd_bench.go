// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func newBenchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bench s3://bucket/key",
		Short: "Benchmark read patterns against S3 (M2)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("bench: not implemented in this build")
		},
	}
}
