// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Daemonization uses a re-exec with a readiness pipe: the parent starts a
// detached child (new session), waits for it to report the mount is ready over
// fd 3, then exits. The child serves in the foreground of its own session.

const (
	daemonChildEnv = "LITH_DAEMON_CHILD"
	readyFD        = 3
)

func isDaemonChild() bool { return os.Getenv(daemonChildEnv) == "1" }

// openOwnedLog opens the per-uid daemon log 0600 with O_NOFOLLOW (no symlink),
// then fstats the opened descriptor and refuses it unless it is a regular file
// owned by our euid. This defeats an attacker who pre-plants a regular file at
// the predictable /tmp path so a root daemon would append diagnostics to a file
// it does not own (info disclosure).
func openOwnedLog(logPath string) (*os.File, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("daemon log %s is not a regular file", logPath)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("daemon log %s: cannot verify ownership", logPath)
	}
	if st.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, fmt.Errorf("daemon log %s is not owned by uid %d", logPath, os.Geteuid())
	}
	return f, nil
}

// daemonize re-execs this process detached and returns once the child signals
// readiness (or fails).
func daemonize() error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Per-uid log opened 0600 with O_NOFOLLOW: a world-writable predictable log
	// in a shared /tmp would let another user pre-plant a symlink so a root
	// daemon appends to an arbitrary file (finding M2). O_NOFOLLOW blocks a
	// symlink but NOT a regular file an attacker pre-created at the predictable
	// path, so after opening we fstat and refuse to append unless the file is a
	// regular file owned by our euid. The path uses Geteuid for consistency with
	// the ownership checks used for mount records.
	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("lith-%d-mount.log", os.Geteuid()))
	logFile, err := openOwnedLog(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1")
	cmd.Stdin = nil
	// Detach the child's output to a log file so it does not hold open the
	// parent's terminal (which would keep an SSH session alive).
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	_ = w.Close() // parent keeps only the read end

	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "ready" {
		fmt.Printf("lith mounted in the background (pid %d); logs: %s\n", cmd.Process.Pid, logPath)
		return nil
	}
	return fmt.Errorf("daemon failed to start (see %s): %s", logPath, line)
}

// signalDaemonReady tells the parent (if we are a daemon child) that the mount
// is up, then detaches from the readiness pipe.
func signalDaemonReady() {
	if !isDaemonChild() {
		return
	}
	f := os.NewFile(readyFD, "ready-pipe")
	if f == nil {
		return
	}
	_, _ = f.WriteString("ready\n")
	_ = f.Close()
}
