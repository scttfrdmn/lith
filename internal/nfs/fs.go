// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	billy "github.com/go-git/go-billy/v5"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
)

// roFS is a read-only billy.Filesystem over lith's Index + BlockStore. It holds
// per-path sequential-read state so the NFS read path drives lith's block
// prefetch (the FUSE layer's job, absent from the naive spike) — that is what
// lifts throughput off the round-trip-bound floor.
type roFS struct {
	cfg    Config
	srv    *server
	ctx    context.Context
	mu     sync.Mutex
	states map[string]*seqState
}

// seqState tracks one file's sequential-read cursor and readahead frontier. It
// is keyed by path: a single client streaming a file maps to one path, and the
// 8-distinct-object measurement gives each object its own state. (Two clients
// reading the same file share this cursor — a known limitation of go-nfs's
// stateless read path; the shared block cache still serves both via
// singleflight. See the PR notes.)
type seqState struct {
	mu       sync.Mutex
	lastEnd  int64 // end offset of the previous read
	frontier int64 // next block index not yet prefetched
}

func (f *roFS) stateFor(p string) *seqState {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.states[p]
	if s == nil {
		s = &seqState{}
		f.states[p] = s
	}
	return s
}

var _ billy.Filesystem = (*roFS)(nil)

// Capabilities advertises a read-only, seekable filesystem so go-nfs rejects
// every write op with NFS3ERR_ROFS before it reaches us.
func (f *roFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

func norm(p string) string { return "/" + strings.TrimPrefix(path.Clean("/"+p), "/") }

func (f *roFS) key(vpath string) blockstore.Key {
	rel := strings.TrimPrefix(vpath, "/")
	k := blockstore.Key{Key: f.cfg.Index.Prefix() + rel, ETagHash: f.cfg.Index.ETagHashOf(vpath)}
	if b, ok := f.cfg.Index.BackingOf(vpath); ok && len(b.Parts) == 1 {
		p := b.Parts[0]
		k.Key = p.ChunkKey
		k.ETagHash = p.ChunkETagHash
		if len(p.Frames) > 0 {
			k.Cargo = &blockstore.CargoChunk{Frames: p.Frames, UncompTotal: p.ChunkUncompTotal}
		}
	}
	return k
}

func (f *roFS) Stat(filename string) (os.FileInfo, error) {
	f.op("getattr")
	vp := norm(filename)
	fi, err := f.cfg.Index.Stat(vp)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &roInfo{name: path.Base(vp), fi: fi}, nil
}

func (f *roFS) Lstat(filename string) (os.FileInfo, error) { return f.Stat(filename) }

func (f *roFS) Open(filename string) (billy.File, error) { return f.OpenFile(filename, os.O_RDONLY, 0) }

func (f *roFS) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission
	}
	vp := norm(filename)
	fi, err := f.cfg.Index.Stat(vp)
	if err != nil || fi.IsDir {
		return nil, os.ErrNotExist
	}
	return &roFile{fs: f, vpath: vp, key: f.key(vp), size: fi.Size, st: f.stateFor(vp)}, nil
}

func (f *roFS) ReadDir(p string) ([]os.FileInfo, error) {
	f.op("readdirplus")
	vp := norm(p)
	var out []os.FileInfo
	var cursor uint64
	for {
		ents, next, err := f.cfg.Index.Readdir(vp, cursor, 4096)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			fi, serr := f.cfg.Index.Stat(path.Join(vp, e.Name))
			if serr != nil {
				continue
			}
			out = append(out, &roInfo{name: e.Name, fi: fi})
		}
		if next == 0 || len(ents) == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}

func (f *roFS) op(name string) {
	if f.cfg.Metrics != nil {
		f.cfg.Metrics.NFSOp(name)
	}
}

func (f *roFS) Join(elem ...string) string { return path.Join(elem...) }
func (f *roFS) Root() string               { return "/" }

func (f *roFS) Chroot(string) (billy.Filesystem, error) { return f, nil }

// Read-only: every mutating op fails (belt-and-suspenders behind Capabilities).
func (f *roFS) Create(string) (billy.File, error)           { return nil, os.ErrPermission }
func (f *roFS) Rename(string, string) error                 { return os.ErrPermission }
func (f *roFS) Remove(string) error                         { return os.ErrPermission }
func (f *roFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (f *roFS) TempFile(string, string) (billy.File, error) { return nil, os.ErrPermission }
func (f *roFS) Symlink(string, string) error                { return os.ErrPermission }
func (f *roFS) Readlink(string) (string, error)             { return "", os.ErrInvalid }

// roFile is a read-only billy.File served from the BlockStore with per-path
// sequential readahead.
type roFile struct {
	fs    *roFS
	vpath string
	key   blockstore.Key
	size  int64
	st    *seqState
	off   int64
}

func (r *roFile) Name() string { return r.vpath }

func (r *roFile) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}

// ReadAt serves [off, off+len) and, when the access looks sequential, dispatches
// block-aligned readahead ahead of the cursor (bounded by the per-client
// fair-share window) so successive reads hit warm or in-flight cache instead of
// paying a round trip each — the fix for the spike's 20 MB/s floor.
func (r *roFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	bs := r.fs.cfg.Store
	blk := bs.BlockSize()

	r.st.mu.Lock()
	seq := off == r.st.lastEnd || (off > r.st.lastEnd && off-r.st.lastEnd < blk)
	r.st.lastEnd = off + int64(len(p))
	var lo, hi int64 = 0, -1
	if seq {
		cur := off / blk
		target := cur + r.fs.srv.windowBlocks()
		if r.st.frontier < cur+1 {
			r.st.frontier = cur + 1
		}
		if r.st.frontier <= target {
			lo, hi = r.st.frontier, target
			r.st.frontier = target + 1
		}
	}
	r.st.mu.Unlock()

	if hi >= lo && hi >= 0 {
		// Dispatch each block's readahead CONCURRENTLY: Prefetch blocks until its
		// block is filled, so a serial loop would fetch one block at a time and
		// throttle a cold single stream (the session-40 finding: 67 MB/s vs the
		// FUSE mount's 787). One goroutine per block lets the block store's own
		// prefetch semaphore bound in-flight depth, matching the FUSE prefetcher's
		// concurrency.
		for b := lo; b <= hi; b++ {
			if b*blk >= r.size {
				break
			}
			b := b
			go r.fs.cfg.Store.Prefetch(r.fs.ctx, r.key, b, r.size)
		}
	}

	// Serve via Chunk() per 1 MiB cache chunk — the same caching read primitive
	// the FUSE layer uses, so bytes are cached (warm re-reads and other clients
	// hit cache) and in-flight fills are joined via singleflight. GetRange is a
	// one-shot bulk helper that does not persist chunks.
	end := off + int64(len(p))
	if end > r.size {
		end = r.size
	}
	n := 0
	for pos := off; pos < end; {
		ci := pos / blockstore.ChunkSize
		cstart := ci * blockstore.ChunkSize
		lo := pos - cstart
		hi := end - cstart
		if hi > blockstore.ChunkSize {
			hi = blockstore.ChunkSize
		}
		buf, err := bs.Chunk(r.fs.ctx, r.key, ci, r.size, lo, hi, seq)
		if err != nil {
			if n > 0 {
				break
			}
			return 0, err
		}
		if hi > int64(len(buf)) {
			hi = int64(len(buf))
		}
		n += copy(p[n:], buf[lo:hi])
		pos = cstart + hi
		if hi < blockstore.ChunkSize && cstart+hi >= r.size {
			break
		}
	}
	if r.fs.cfg.Metrics != nil {
		r.fs.cfg.Metrics.NFSOp("read")
		r.fs.cfg.Metrics.NFSReadBytes(int64(n))
	}
	if off+int64(n) >= r.size || n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *roFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = offset
	case io.SeekCurrent:
		r.off += offset
	case io.SeekEnd:
		r.off = r.size + offset
	}
	return r.off, nil
}

func (r *roFile) Write([]byte) (int, error) { return 0, os.ErrPermission }
func (r *roFile) Close() error              { return nil }
func (r *roFile) Lock() error               { return nil }
func (r *roFile) Unlock() error             { return nil }
func (r *roFile) Truncate(int64) error      { return os.ErrPermission }

// roInfo adapts index.FileInfo to os.FileInfo.
type roInfo struct {
	name string
	fi   index.FileInfo
}

func (i *roInfo) Name() string { return i.name }
func (i *roInfo) Size() int64  { return i.fi.Size }
func (i *roInfo) Mode() iofs.FileMode {
	if i.fi.IsDir {
		return iofs.ModeDir | 0o555
	}
	return 0o444
}
func (i *roInfo) ModTime() time.Time { return i.fi.MTime }
func (i *roInfo) IsDir() bool        { return i.fi.IsDir }
func (i *roInfo) Sys() any           { return nil }
