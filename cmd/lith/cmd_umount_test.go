// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"
)

// withHooks saves/restores the injectable umount hooks around a test.
func withHooks(t *testing.T) {
	t.Helper()
	sm, fu, fh, pr := signalMount, fusermountUnmount, fuserHolders, procMountsReader
	t.Cleanup(func() { signalMount, fusermountUnmount, fuserHolders, procMountsReader = sm, fu, fh, pr })
}

func TestMountRecordRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rec := mountRecord{PID: 4242, Mountpoint: "/mnt/x", Bucket: "b", Root: "a/b/", IndexFile: "/i.idx", Start: time.Now()}
	if _, err := writeMountRecord(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := listMountRecords()
	if len(got) != 1 || got[0].PID != 4242 || got[0].Root != "a/b/" {
		t.Fatalf("listMountRecords = %+v", got)
	}
	removeMountRecord("/mnt/x")
	if len(listMountRecords()) != 0 {
		t.Fatal("record not removed")
	}
}

func TestParseProcMounts(t *testing.T) {
	in := "s3://b /mnt/x fuse.lith rw 0 0\n/dev/root / ext4 rw 0 0\nfoo /mnt/y\\040z fuse rw 0 0\n"
	pms := parseProcMounts(strings.NewReader(in))
	if len(pms) != 3 || !isLithMount(pms[0]) || isLithMount(pms[1]) {
		t.Fatalf("parse = %+v", pms)
	}
	if pms[2].Target != "/mnt/y z" { // octal-escape decode
		t.Fatalf("escape decode = %q", pms[2].Target)
	}
}

// TestUmountCleanSIGTERM: SIGTERM makes the mount disappear before the timeout.
func TestUmountCleanSIGTERM(t *testing.T) {
	withHooks(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, _ = writeMountRecord(mountRecord{PID: 111, Mountpoint: "/mnt/x", Bucket: "b"})
	mounted := true
	procMountsReader = func() []procMount {
		if mounted {
			return []procMount{{Source: "s3://b", Target: "/mnt/x", Fstype: "fuse.lith"}}
		}
		return nil
	}
	var gotSig int
	signalMount = func(pid int, sig syscall.Signal) error { gotSig = pid; mounted = false; return nil }
	fusermountUnmount = func(string, bool) error { t.Fatal("fusermount should not be called on a clean SIGTERM"); return nil }

	var out bytes.Buffer
	if err := umountOne(&out, "/mnt/x", time.Second, false); err != nil {
		t.Fatalf("umountOne: %v", err)
	}
	if gotSig != 111 || !strings.Contains(out.String(), "SIGTERM") {
		t.Fatalf("sig=%d out=%q", gotSig, out.String())
	}
	if len(listMountRecords()) != 0 {
		t.Fatal("record not removed after unmount")
	}
}

// TestUmountTimeoutFallback: SIGTERM doesn't clear it; fusermount3 -u does.
func TestUmountTimeoutFallback(t *testing.T) {
	withHooks(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, _ = writeMountRecord(mountRecord{PID: 222, Mountpoint: "/mnt/x"})
	mounted := true
	procMountsReader = func() []procMount {
		if mounted {
			return []procMount{{Target: "/mnt/x", Fstype: "fuse.lith"}}
		}
		return nil
	}
	signalMount = func(int, syscall.Signal) error { return nil } // process ignores SIGTERM
	fuserHolders = func(string) []int { return nil }
	var lazy []bool
	fusermountUnmount = func(_ string, l bool) error { lazy = append(lazy, l); mounted = false; return nil }

	var out bytes.Buffer
	if err := umountOne(&out, "/mnt/x", 300*time.Millisecond, false); err != nil {
		t.Fatalf("umountOne: %v", err)
	}
	if len(lazy) != 1 || lazy[0] { // exactly one call, non-lazy
		t.Fatalf("fusermount calls = %v, want one non-lazy", lazy)
	}
	if !strings.Contains(out.String(), "fusermount3 -u") {
		t.Fatalf("out = %q", out.String())
	}
}

// TestUmountBusyRefusal: process gone, files held; refuse without --force.
func TestUmountBusyRefusal(t *testing.T) {
	withHooks(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	procMountsReader = func() []procMount {
		return []procMount{{Target: "/mnt/x", Fstype: "fuse.lith"}} // always mounted
	}
	signalMount = func(int, syscall.Signal) error { return nil }
	fuserHolders = func(string) []int { return []int{9001, 9002} }
	fusermountUnmount = func(string, bool) error { t.Fatal("must not unmount a busy mount without --force"); return nil }

	var out bytes.Buffer
	err := umountOne(&out, "/mnt/x", 200*time.Millisecond, false)
	if err == nil || !strings.Contains(err.Error(), "busy") || !strings.Contains(err.Error(), "9001") {
		t.Fatalf("err = %v, want busy naming pids", err)
	}
}
