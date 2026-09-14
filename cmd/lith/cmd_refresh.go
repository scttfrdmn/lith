// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"syscall"

	"github.com/spf13/cobra"
)

// newRefreshCmd builds `lith refresh MOUNTPOINT`: re-read a published dataset's
// CURRENT pointer and hot-swap the live mount to the new version (#167). It is
// explicit — there is no auto-polling. It signals the running mount process
// (SIGHUP); the mount re-resolves @current and, if the version changed, atomically
// swaps its index. Open handles keep the version they resolved against.
func newRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh MOUNTPOINT",
		Short: "Re-read CURRENT and hot-swap a published-dataset mount to the new version",
		Long: `Re-read the CURRENT pointer of a dataset mounted with s3://bucket/dataset@current
and, if a new version has been published, atomically swap the mount to it.

Explicit only — lith never polls. Open file handles keep the version they were
opened against (versions are immutable, so their objects still exist); only new
lookups see the new version.

Applies to a single-client FUSE mount. The NFS gateway does not hot-swap — a
version change there STALEs every client handle, so the gateway must be restarted
to adopt a new version (see the Published datasets docs).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mountpoint := args[0]
			rec, err := readMountRecord(mountpoint)
			if err != nil {
				return fmt.Errorf("no lith mount record for %q: %w", mountpoint, err)
			}
			// Refuse anything that is not a published-dataset (@ref) mount: those
			// mounts install no SIGHUP handler, and SIGHUP's default action would
			// TERMINATE the process. An @ref mount always records a resolved version.
			if rec.Version == "" {
				return fmt.Errorf("%q is not a published-dataset mount (no @current/@version); refresh only applies to s3://bucket/dataset@current mounts", mountpoint)
			}
			if rec.PID <= 0 {
				return fmt.Errorf("mount record for %q has no pid", mountpoint)
			}
			if err := syscall.Kill(rec.PID, syscall.SIGHUP); err != nil {
				return fmt.Errorf("signal mount pid %d: %w", rec.PID, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "refresh signalled to lith mount at %s (pid %d, version %s); the mount log reports the swap\n",
				mountpoint, rec.PID, rec.Version)
			return nil
		},
	}
}
