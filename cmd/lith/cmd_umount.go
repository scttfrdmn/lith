// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// trustedExecDirs are the only directories lith will exec system helpers from.
// Resolving against a fixed list (not $PATH) stops a poisoned PATH under `sudo`
// without secure_path from running an attacker binary as root (finding L7).
var trustedExecDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// trustedExecPath returns the first existing absolute path for name in
// trustedExecDirs, or an error if none is found.
func trustedExecPath(name string) (string, error) {
	for _, d := range trustedExecDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in %v", name, trustedExecDirs)
}

// Injectable hooks (overridden in tests).
var (
	signalMount = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
	// fusermountUnmount runs `fusermount3 -u` (or -uz when lazy).
	fusermountUnmount = func(mountpoint string, lazy bool) error {
		flag := "-u"
		if lazy {
			flag = "-uz"
		}
		bin, err := trustedExecPath("fusermount3")
		if err != nil {
			return err
		}
		out, err := exec.Command(bin, flag, mountpoint).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// fuserHolders returns the pids with open files under the mountpoint.
	fuserHolders = func(mountpoint string) []int {
		bin, err := trustedExecPath("fuser")
		if err != nil {
			return nil
		}
		out, _ := exec.Command(bin, "-m", mountpoint).Output()
		var pids []int
		for _, tok := range strings.Fields(string(out)) {
			if p, err := strconv.Atoi(tok); err == nil {
				pids = append(pids, p)
			}
		}
		return pids
	}
)

func newMountsCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "mounts",
		Short: "List live lith mounts (and stale records)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			recs := listMountRecords()
			seen := map[string]bool{}
			_, _ = fmt.Fprintf(out, "%-24s %-10s %-14s %-8s %s\n", "MOUNTPOINT", "STATUS", "BUCKET", "PID", "ROOT / INDEX")
			for _, r := range recs {
				abs, _ := filepath.Abs(r.Mountpoint)
				seen[abs] = true
				status := "stale"
				if mountedAt(r.Mountpoint) {
					status = "live"
				} else if prune {
					removeMountRecord(r.Mountpoint)
					status = "pruned"
				}
				root := r.Root
				if root == "" {
					root = "(bucket root)"
				}
				source := r.IndexFile
				if r.Manifest != "" {
					source = "cargoship:" + r.Manifest
				}
				_, _ = fmt.Fprintf(out, "%-24s %-10s %-14s %-8d %s  [%s]\n", r.Mountpoint, status, r.Bucket, r.PID, root, source)
			}
			// lith mounts in /proc with no record (started elsewhere).
			for _, pm := range procMountsReader() {
				if isLithMount(pm) && !seen[pm.Target] {
					_, _ = fmt.Fprintf(out, "%-24s %-10s %-14s %-8s %s\n", pm.Target, "live", pm.Source, "?", "(no record)")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "remove records whose mount is no longer live")
	return cmd
}

func newUmountCmd() *cobra.Command {
	var (
		all     bool
		force   bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "umount MOUNTPOINT",
		Short: "Unmount a lith filesystem (SIGTERM, then fusermount fallback)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			var targets []string
			if all {
				targets = allLithMountpoints()
			} else {
				if len(args) != 1 {
					return fmt.Errorf("umount takes one MOUNTPOINT (or --all)")
				}
				abs, _ := filepath.Abs(args[0])
				targets = []string{abs}
			}
			if len(targets) == 0 {
				_, _ = fmt.Fprintln(out, "no lith mounts")
				return nil
			}
			failed := 0
			for _, mp := range targets {
				if err := umountOne(out, mp, timeout, force); err != nil {
					_, _ = fmt.Fprintf(out, "%s: %v\n", mp, err)
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d mount(s) still mounted", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "unmount every lith mount for this user")
	cmd.Flags().BoolVar(&force, "force", false, "escalate to a lazy unmount (-uz) if a clean unmount fails")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "how long to wait for the mount process to exit after SIGTERM")
	return cmd
}

// allLithMountpoints is the union of recorded mountpoints and /proc lith mounts.
func allLithMountpoints() []string {
	seen := map[string]bool{}
	var out []string
	add := func(mp string) {
		abs, _ := filepath.Abs(mp)
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	for _, r := range listMountRecords() {
		add(r.Mountpoint)
	}
	for _, pm := range procMountsReader() {
		if isLithMount(pm) {
			add(pm.Target)
		}
	}
	return out
}

// umountOne unmounts a single lith mountpoint. Order: SIGTERM the owning pid and
// wait; then `fusermount3 -u`; then `-uz` (lazy) only with force. A busy mount
// (open files, process gone) is reported and refused unless force. Returns an
// error if the mountpoint is still mounted at the end.
func umountOne(out interface{ Write([]byte) (int, error) }, mp string, timeout time.Duration, force bool) error {
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	if !mountedAt(mp) {
		removeMountRecord(mp)
		pf("%s: not mounted\n", mp)
		return nil
	}
	var pid int
	// listMountRecords only returns records from a run dir we own (finding H2),
	// but even a trusted record's pid is not proof it owns this mount — verify it
	// against the processes actually holding the mount open before signalling.
	for _, r := range listMountRecords() {
		if abs, _ := filepath.Abs(r.Mountpoint); abs == mp {
			pid = r.PID
		}
	}
	holders := fuserHolders(mp)

	// SIGTERM the owning process only if it genuinely holds this mount; never
	// signal a pid we cannot verify (it may be forged or unrelated).
	if pid > 0 && containsInt(holders, pid) {
		_ = signalMount(pid, syscall.SIGTERM)
		if waitUnmounted(mp, timeout) {
			removeMountRecord(mp)
			pf("%s: unmounted (SIGTERM)\n", mp)
			return nil
		}
	}

	// Still mounted. If files are open, refuse unless force.
	if len(holders) > 0 && !force {
		return fmt.Errorf("busy — open files held by pids %v (retry with --force to lazy-unmount)", holders)
	}

	// Clean unmount attempt.
	if err := fusermountUnmount(mp, false); err == nil && !mountedAt(mp) {
		removeMountRecord(mp)
		pf("%s: unmounted (fusermount3 -u)\n", mp)
		return nil
	}
	// Lazy unmount only with force.
	if force {
		if err := fusermountUnmount(mp, true); err == nil && !mountedAt(mp) {
			removeMountRecord(mp)
			pf("%s: unmounted (fusermount3 -uz, lazy)\n", mp)
			return nil
		}
	}
	if mountedAt(mp) {
		return fmt.Errorf("still mounted (try --force)")
	}
	removeMountRecord(mp)
	return nil
}

// containsInt reports whether n is in s.
func containsInt(s []int, n int) bool {
	for _, v := range s {
		if v == n {
			return true
		}
	}
	return false
}

// waitUnmounted polls until the mount leaves /proc/mounts or the timeout.
func waitUnmounted(mp string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !mountedAt(mp) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
