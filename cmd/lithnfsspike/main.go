// SPDX-License-Identifier: Apache-2.0

// Command lithnfsspike is a SESSION-38 PROTOTYPE SPIKE ONLY (not for merge): a
// native Go NFSv3 server (option b, #143/#144) that serves lith's Index +
// BlockStore directly over willscott/go-nfs, with no FUSE in the read path. It
// exists to measure the native path against nfs-ganesha-over-FUSE (option a);
// it is intentionally minimal (read-only, no fair-share, no handle-sha, smoke
// only). Do not build the real gateway from this file.
package main

import (
	"context"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"time"

	billy "github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client"
)

func main() {
	var (
		idxPath = flag.String("index-file", "", "lith index file to serve")
		bucket  = flag.String("bucket", "", "S3 bucket")
		region  = flag.String("region", "us-east-1", "region")
		addr    = flag.String("addr", ":2049", "listen address (NFS + embedded MOUNT)")
		noSign  = flag.Bool("no-sign-request", false, "anonymous S3")
		mem     = flag.Int64("mem-cache", 8<<30, "memory cache bytes")
	)
	flag.Parse()
	if *idxPath == "" || *bucket == "" {
		log.Fatal("need --index-file and --bucket")
	}
	ctx := context.Background()
	ix, closeFn, err := index.Open(*idxPath)
	if err != nil {
		log.Fatalf("open index: %v", err)
	}
	defer func() { _ = closeFn() }()
	client, err := s3client.New(ctx, s3client.Config{Bucket: *bucket, Region: *region, NoSignRequest: *noSign, Concurrency: 128})
	if err != nil {
		log.Fatalf("s3 client: %v", err)
	}
	bs, err := blockstore.New(client, blockstore.Config{Bucket: *bucket, BlockSize: 8 << 20, MemCache: *mem, MaxRange: 64 << 20})
	if err != nil {
		log.Fatalf("blockstore: %v", err)
	}
	fsys := &roFS{ix: ix, bs: bs, ctx: ctx}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	handler := nfshelper.NewNullAuthHandler(fsys)
	cache := nfshelper.NewCachingHandler(handler, 4096)
	log.Printf("lithnfsspike serving %d keys on %s", ix.Len(), *addr)
	if err := nfs.Serve(ln, cache); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// --- read-only billy.Filesystem over Index + BlockStore ---

type roFS struct {
	ix  index.Reader
	bs  *blockstore.BlockStore
	ctx context.Context
}

// norm turns a billy path into the index's "/"-rooted form.
func norm(p string) string {
	p = "/" + strings.TrimPrefix(path.Clean("/"+p), "/")
	return p
}

func (f *roFS) key(vpath string) blockstore.Key {
	rel := strings.TrimPrefix(vpath, "/")
	k := blockstore.Key{Key: f.ix.Prefix() + rel, ETagHash: f.ix.ETagHashOf(vpath)}
	if b, ok := f.ix.BackingOf(vpath); ok && len(b.Parts) == 1 {
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
	vp := norm(filename)
	fi, err := f.ix.Stat(vp)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &roInfo{name: path.Base(vp), fi: fi}, nil
}

func (f *roFS) Lstat(filename string) (os.FileInfo, error) { return f.Stat(filename) }

func (f *roFS) Open(filename string) (billy.File, error) { return f.OpenFile(filename, os.O_RDONLY, 0) }

func (f *roFS) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission // read-only
	}
	vp := norm(filename)
	fi, err := f.ix.Stat(vp)
	if err != nil || fi.IsDir {
		return nil, os.ErrNotExist
	}
	return &roFile{fs: f, vpath: vp, key: f.key(vp), size: fi.Size}, nil
}

func (f *roFS) ReadDir(p string) ([]os.FileInfo, error) {
	vp := norm(p)
	var out []os.FileInfo
	var cursor uint64
	for {
		ents, next, err := f.ix.Readdir(vp, cursor, 4096)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			cp := path.Join(vp, e.Name)
			fi, serr := f.ix.Stat(cp)
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

func (f *roFS) Join(elem ...string) string { return path.Join(elem...) }
func (f *roFS) Root() string               { return "/" }

// Chroot is used by go-nfs to scope an export; we serve the whole index.
func (f *roFS) Chroot(string) (billy.Filesystem, error) { return f, nil }

// Read-only: every mutating op fails.
func (f *roFS) Create(string) (billy.File, error)           { return nil, os.ErrPermission }
func (f *roFS) Rename(string, string) error                 { return os.ErrPermission }
func (f *roFS) Remove(string) error                         { return os.ErrPermission }
func (f *roFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (f *roFS) TempFile(string, string) (billy.File, error) { return nil, os.ErrPermission }
func (f *roFS) Symlink(string, string) error                { return os.ErrPermission }
func (f *roFS) Readlink(string) (string, error)             { return "", os.ErrInvalid }

// roFile is a read-only billy.File served from the BlockStore.
type roFile struct {
	fs    *roFS
	vpath string
	key   blockstore.Key
	size  int64
	off   int64
}

func (r *roFile) Name() string { return r.vpath }

func (r *roFile) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}

func (r *roFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	data, err := r.fs.bs.GetRange(r.fs.ctx, r.key, off, int64(len(p)), r.size)
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if int64(off)+int64(n) >= r.size {
		return n, io.EOF
	}
	if n < len(p) {
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
func (i *roInfo) Mode() fs.FileMode {
	if i.fi.IsDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i *roInfo) ModTime() time.Time { return i.fi.MTime }
func (i *roInfo) IsDir() bool        { return i.fi.IsDir }
func (i *roInfo) Sys() any           { return nil }
