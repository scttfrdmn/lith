// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	gonfs "github.com/willscott/go-nfs"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// testGateway builds an index + blockstore over an in-memory fake and returns a
// wired handler/roFS/server for unit tests (no real NFS network).
func testGateway(t *testing.T, objs map[string][]byte) (*handler, *fake.Server) {
	t.Helper()
	srv := fake.New()
	for k, v := range objs {
		srv.Put(k, v, time.Unix(1_700_000_000, 0))
	}
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{
		Options: index.Options{Bucket: "bkt", Prefix: ""},
	})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	reader, err := ix.Root("")
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	// MemCache is sharded 64 ways, so it must be large enough that a shard holds
	// the test's chunks (a real gateway uses GBs). 1 GiB => 16 MiB/shard.
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 1 << 20, MemCache: 1 << 30, MaxRange: 64 << 20})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	t.Cleanup(bs.Close)
	cfg := Config{Index: reader, Store: bs, RootID: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, ClientIdle: time.Minute}
	s := &server{cfg: cfg, clients: map[string]time.Time{}}
	fs := &roFS{cfg: cfg, srv: s, ctx: context.Background(), states: map[string]*seqState{}}
	return &handler{cfg: cfg, srv: s, fs: fs}, srv
}

func TestHandleRoundTripAndStale(t *testing.T) {
	h, _ := testGateway(t, map[string][]byte{"a/b.txt": []byte("hello")})

	// path -> handle -> path
	fh := h.ToHandle(h.fs, []string{"a", "b.txt"})
	if len(fh) != 16 {
		t.Fatalf("handle is %d bytes, want 16", len(fh))
	}
	_, parts, err := h.FromHandle(fh)
	if err != nil || strings.Join(parts, "/") != "a/b.txt" {
		t.Fatalf("FromHandle = %v,%v; want a/b.txt", parts, err)
	}

	// Wrong root id -> STALE.
	bad := append([]byte(nil), fh...)
	bad[0] ^= 0xff
	if _, _, err := h.FromHandle(bad); !isStatus(err, gonfs.NFSStatusStale) {
		t.Errorf("mismatched root id: err=%v, want STALE", err)
	}
	// Unknown inode (valid root id) -> STALE.
	unknown := append([]byte(nil), fh...)
	for i := 8; i < 16; i++ {
		unknown[i] = 0xff
	}
	if _, _, err := h.FromHandle(unknown); !isStatus(err, gonfs.NFSStatusStale) {
		t.Errorf("unknown inode: err=%v, want STALE", err)
	}
	// Wrong length -> STALE.
	if _, _, err := h.FromHandle([]byte{1, 2, 3}); !isStatus(err, gonfs.NFSStatusStale) {
		t.Errorf("short handle: err=%v, want STALE", err)
	}
}

func isStatus(err error, want gonfs.NFSStatus) bool {
	var se *gonfs.NFSStatusError
	if errors.As(err, &se) {
		return se.NFSStatus == want
	}
	return false
}

func TestReadOnly(t *testing.T) {
	h, _ := testGateway(t, map[string][]byte{"f.txt": []byte("x")})
	fs := h.fs
	// Capabilities exclude write, so go-nfs rejects mutating RPCs with ROFS.
	if fs.Capabilities()&billy.WriteCapability != 0 {
		t.Error("filesystem advertises write capability")
	}
	// The mutating methods themselves error.
	if _, err := fs.Create("g"); err == nil {
		t.Error("Create should fail")
	}
	if err := fs.Rename("a", "b"); err == nil {
		t.Error("Rename should fail")
	}
	if err := fs.Remove("f.txt"); err == nil {
		t.Error("Remove should fail")
	}
	if err := fs.MkdirAll("d", 0o777); err == nil {
		t.Error("MkdirAll should fail")
	}
	if err := fs.Symlink("a", "b"); err == nil {
		t.Error("Symlink should fail")
	}
	if fl, err := fs.OpenFile("f.txt", os.O_RDWR, 0); err == nil {
		_ = fl
		t.Error("OpenFile for write should fail")
	}
	f, err := fs.Open("f.txt")
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Error("Write should fail")
	}
	if err := f.Truncate(0); err == nil {
		t.Error("Truncate should fail")
	}
}

func TestFairShareArithmetic(t *testing.T) {
	h, _ := testGateway(t, map[string][]byte{"f.txt": []byte("x")})
	s := h.srv
	budget := s.cfg.Store.PrefetchBudgetBlocks()
	if budget < 8 {
		t.Skipf("prefetch budget %d blocks too small for the 8-client assertion", budget)
	}
	// window = min(budget/clients, cap) with a floor of 2. The cap bounds
	// per-stream readahead dispatch; below it the share divides by client count.
	const cap, floor = 96, 2
	want := func(n int64) int64 {
		w := budget / n
		if w > cap {
			w = cap
		}
		if w < floor {
			w = floor
		}
		return w
	}
	s.mountClient("10.0.0.1")
	if w := s.windowBlocks(); w != want(1) {
		t.Errorf("1 client window = %d, want %d", w, want(1))
	}
	s.mountClient("10.0.0.2")
	if w := s.windowBlocks(); w != want(2) {
		t.Errorf("2 clients window = %d, want %d", w, want(2))
	}
	for i := 3; i <= 8; i++ {
		s.mountClient("10.0.0." + string(rune('0'+i)))
	}
	if w := s.windowBlocks(); w != want(8) {
		t.Errorf("8 clients window = %d, want %d", w, want(8))
	}
}

func TestReadServesBytesAndSharesGET(t *testing.T) {
	// One 4 MiB object; two readers of the same file issue GETs only for the
	// bytes once (shared block cache), not twice.
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i / 4096)
	}
	h, srv := testGateway(t, map[string][]byte{"big.bin": data})
	fs := h.fs

	read := func() []byte {
		f, err := fs.Open("big.bin")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, len(data))
		got := 0
		for got < len(buf) {
			n, err := f.ReadAt(buf[got:], int64(got))
			got += n
			if err != nil {
				break
			}
		}
		return buf[:got]
	}

	first := read()
	if len(first) != len(data) || first[0] != 0 || first[len(first)-1] != data[len(data)-1] {
		t.Fatalf("first read short/mismatch: %d bytes", len(first))
	}
	// Let any lingering async readahead settle so the warm-reread count is not
	// polluted by prefetch GETs still in flight from the first pass.
	quiesce(srv)
	gets := srv.GetCallCount()
	// A second full read is served entirely from cache: no new GETs.
	second := read()
	if string(second) != string(data) {
		t.Fatal("second read mismatch")
	}
	if after := srv.GetCallCount(); after != gets {
		t.Errorf("warm re-read issued %d new GET(s), want 0", after-gets)
	}
}

// quiesce waits until the fake's GET count stops changing (async prefetch has
// drained), so a subsequent warm-read count is deterministic.
func quiesce(srv *fake.Server) {
	last := int64(-1)
	for i := 0; i < 100; i++ {
		n := int64(srv.GetCallCount())
		if n == last {
			return
		}
		last = n
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentReadersRace(t *testing.T) {
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte(i)
	}
	h, _ := testGateway(t, map[string][]byte{"o0.bin": data, "o1.bin": data, "o2.bin": data})
	fs := h.fs
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := fs.Open("o" + string(rune('0'+i%3)) + ".bin")
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			defer func() { _ = f.Close() }()
			buf := make([]byte, 64<<10)
			_, _ = f.ReadAt(buf, int64((i%16)*4096))
		}(i)
	}
	wg.Wait()
}
