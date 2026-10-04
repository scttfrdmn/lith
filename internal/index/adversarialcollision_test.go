// SPDX-License-Identifier: Apache-2.0

package index

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// M17-B case 1 (#217): an index whose inode hashes COLLIDE HEAVILY — "force collisions;
// confirm the fallback holds UNDER LOAD, not just in a unit test".
//
// The existing coverage is two single-threaded unit tests: TestInodeCollisionFallback seeds
// the used set and checks one probe, and TestByInodeCollisionsResolved maps every key to the
// same hash. Neither exercises the thing that actually runs in production — ByInode's lazy
// index, built once under sync.Once on whichever caller gets there first, while other
// goroutines are resolving handles against it.
//
// The property under test is a BIJECTION: every path maps to exactly one inode, and that
// inode maps back to exactly that path. The inode is what an NFS file handle carries
// (#144), so a collision resolved wrongly means a handle resolving to ANOTHER FILE — the
// same "serves wrong" shape as #343, which was this campaign's first finding.
func TestInodeBijectionHoldsUnderHeavyCollisionsAndConcurrency(t *testing.T) {
	orig := hashKey
	// A 10-bit hash over 8000 keys: ~1024 natural values for 8000 files plus their
	// directories, so virtually every assignment after the first thousand probes, and the
	// probe chains are long rather than incidental.
	hashKey = func(s string) uint64 { return orig(s) & 0x3ff }
	defer func() { hashKey = orig }()

	const nFiles = 8000
	entries := make([]Entry, 0, nFiles)
	for i := range nFiles {
		// Spread across directories so the shared file/directory inode namespace is
		// genuinely contested, not just the file half.
		entries = append(entries, Entry{
			Key:   fmt.Sprintf("d%d/s%d/f%06d.bin", i%37, i%11, i),
			Size:  int64(i) + 1,
			MTime: int64(i) * 1_000_000,
		})
	}
	ix := Build(entries, Options{Bucket: "bkt", Prefix: "root/"})

	// NON-VACUITY: the fixture must actually have collided, or this is a plain bijection
	// test on a hash that never clashed. With 1024 values and >8000 inodes to hand out,
	// the count must be at least nFiles - 1024.
	_, _, collisions := ix.Stats()
	if collisions < nFiles-1024 {
		t.Fatalf("fixture: only %d collisions over %d files; the reduced hash is not "+
			"forcing the fallback path", collisions, nFiles)
	}
	if ix.Len() != nFiles {
		t.Fatalf("index has %d entries, want %d", ix.Len(), nFiles)
	}

	// The expected mapping, computed single-threaded: path -> inode, from Stat.
	want := make(map[string]uint64, nFiles)
	seen := make(map[uint64]string, nFiles)
	for i := range nFiles {
		p := "/" + entries[i].Key
		fi, err := ix.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Ino == 0 || fi.Ino == rootIno {
			t.Fatalf("%s got reserved inode %d", p, fi.Ino)
		}
		if other, dup := seen[fi.Ino]; dup {
			t.Fatalf("verdict SERVES WRONG: %s and %s share inode %d; an NFS handle for "+
				"one resolves to the other", p, other, fi.Ino)
		}
		seen[fi.Ino] = p
		want[p] = fi.Ino
	}

	// UNDER LOAD: many goroutines hit ByInode with the lazy index unbuilt, so they race
	// for the sync.Once and then read the array the winner built. Run with -race; a torn
	// or partially-sorted inoKeys would also show up as a wrong path below.
	const workers = 32
	var wrong, missing atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start // all workers arrive before the first ByInode call
			for i := w; i < nFiles; i += workers {
				p := "/" + entries[i].Key
				got, ok := ix.ByInode(want[p])
				switch {
				case !ok:
					missing.Add(1)
				case got != p:
					wrong.Add(1)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if n := wrong.Load(); n != 0 {
		t.Errorf("verdict SERVES WRONG: %d of %d inodes resolved to a DIFFERENT path "+
			"under concurrent load", n, nFiles)
	}
	if n := missing.Load(); n != 0 {
		t.Errorf("%d of %d inodes did not resolve at all under concurrent load", n, nFiles)
	}

	// Directories share the namespace, so their inodes must be distinct from every file's
	// too — assignIno's `used` set is one map for both, and a per-table set would pass
	// every assertion above while handing a directory a file's inode.
	for i := range nFiles {
		dir := fmt.Sprintf("/d%d/s%d", i%37, i%11)
		fi, err := ix.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir %s: %v", dir, err)
		}
		if p, clash := seen[fi.Ino]; clash {
			t.Fatalf("verdict SERVES WRONG: directory %s and file %s share inode %d",
				dir, p, fi.Ino)
		}
		// And it must resolve back to the directory, not to a file at a nearby probe.
		if got, ok := ix.ByInode(fi.Ino); !ok || got != dir {
			t.Errorf("directory inode %d resolved to %q (ok=%v), want %q", fi.Ino, got, ok, dir)
		}
	}
}

// The same bijection through a SUB-ROOT view, which is the shape a prefix mount uses. The
// view filters by path, so an inode whose probe landed it next to an out-of-view file must
// not leak that file in — and an in-view inode must still resolve.
func TestViewInodeResolutionUnderCollisionsStaysInsideTheView(t *testing.T) {
	orig := hashKey
	hashKey = func(s string) uint64 { return orig(s) & 0x7f } // 128 values, maximal probing
	defer func() { hashKey = orig }()

	var entries []Entry
	for i := range 400 {
		entries = append(entries, Entry{Key: fmt.Sprintf("in/f%03d", i), Size: 1, MTime: 1})
		entries = append(entries, Entry{Key: fmt.Sprintf("out/f%03d", i), Size: 1, MTime: 1})
	}
	ix := Build(entries, Options{Bucket: "bkt"})
	if _, _, c := ix.Stats(); c < 600 {
		t.Fatalf("fixture: %d collisions; the reduced hash is not forcing probing", c)
	}

	v, err := ix.Root("in/")
	if err != nil {
		t.Fatalf("root: %v", err)
	}

	var leaked, lost int
	for i := range 400 {
		// An in-view path must resolve, view-relative.
		p := fmt.Sprintf("/f%03d", i)
		fi, serr := v.Stat(p)
		if serr != nil {
			t.Fatalf("stat %s: %v", p, serr)
		}
		if got, ok := v.ByInode(fi.Ino); !ok || got != p {
			lost++
		}
		// An out-of-view path's inode must NOT resolve through the view, however close
		// its probed value sits to an in-view one.
		ofi, serr := ix.Stat(fmt.Sprintf("/out/f%03d", i))
		if serr != nil {
			t.Fatalf("stat out/f%03d: %v", i, serr)
		}
		if got, ok := v.ByInode(ofi.Ino); ok {
			leaked++
			if i == 0 {
				t.Errorf("an out-of-view inode resolved to %q inside the view", got)
			}
		}
	}
	if lost != 0 {
		t.Errorf("%d of 400 in-view inodes failed to resolve through the view", lost)
	}
	if leaked != 0 {
		t.Errorf("verdict SERVES WRONG: %d of 400 out-of-view inodes resolved inside the "+
			"view; a handle for a file outside the mount would serve through it", leaked)
	}
}
