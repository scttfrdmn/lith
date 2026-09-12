// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"path"
	"strings"
	"time"

	billy "github.com/go-git/go-billy/v5"
	gonfs "github.com/willscott/go-nfs"
)

// handler implements gonfs.Handler with deterministic (root id, inode) file
// handles, replacing go-nfs's ephemeral in-memory CachingHandler. Handles are
// 16 bytes (8-byte root id + 8-byte inode), well under the 64-byte v3 limit,
// stable across a restart against the same index, and NFS3ERR_STALE against a
// rebuilt one (different root id) — no server-side handle table, no LRU
// eviction, no restart remapping.
type handler struct {
	cfg Config
	srv *server
	fs  *roFS
}

var _ gonfs.Handler = (*handler)(nil)

// Mount registers the client (its remote address) for the clients gauge and the
// fair-share window, and returns the shared read-only filesystem. AUTH_UNIX and
// AUTH_NULL are advertised; there is no per-client filesystem because go-nfs
// resolves later ops through FromHandle, which does not carry the client.
func (h *handler) Mount(_ context.Context, conn net.Conn, _ gonfs.MountRequest) (gonfs.MountStatus, billy.Filesystem, []gonfs.AuthFlavor) {
	if conn != nil {
		if ra := conn.RemoteAddr(); ra != nil {
			h.srv.mountClient(clientKey(ra.String()))
		}
	}
	return gonfs.MountStatusOk, h.fs, []gonfs.AuthFlavor{gonfs.AuthFlavorUnix, gonfs.AuthFlavorNull}
}

// Change returns nil: the export is read-only, so go-nfs rejects every mutating
// op with NFS3ERR_ROFS before it reaches the filesystem.
func (h *handler) Change(billy.Filesystem) billy.Change { return nil }

// FSStat reports the export's size from the index; free/available are zero (a
// read-only export never has writable space).
func (h *handler) FSStat(_ context.Context, _ billy.Filesystem, s *gonfs.FSStat) error {
	s.TotalSize = uint64(h.cfg.Index.TotalSize())
	s.FreeSize, s.AvailableSize = 0, 0
	s.TotalFiles = uint64(h.cfg.Index.Len())
	s.FreeFiles, s.AvailableFiles = 0, 0
	s.CacheHint = time.Hour // immutable data: clients may cache attributes long
	return nil
}

// ToHandle resolves a path to its (root id, inode) handle.
func (h *handler) ToHandle(_ billy.Filesystem, pathparts []string) []byte {
	fi, err := h.cfg.Index.Stat(joinAbs(pathparts))
	if err != nil {
		return []byte{} // unresolvable path: an empty handle never matches FromHandle
	}
	b := make([]byte, 16)
	copy(b[0:8], h.cfg.RootID[:])
	binary.BigEndian.PutUint64(b[8:16], fi.Ino)
	return b
}

// FromHandle decodes a handle: a mismatched root id (rebuilt/different index) or
// an inode no entry owns is NFS3ERR_STALE. Otherwise it resolves the inode back
// to a path via the index.
func (h *handler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	if len(fh) != 16 || !bytes.Equal(fh[0:8], h.cfg.RootID[:]) {
		return nil, nil, &gonfs.NFSStatusError{NFSStatus: gonfs.NFSStatusStale}
	}
	ino := binary.BigEndian.Uint64(fh[8:16])
	p, ok := h.cfg.Index.ByInode(ino)
	if !ok {
		return nil, nil, &gonfs.NFSStatusError{NFSStatus: gonfs.NFSStatusStale}
	}
	return h.fs, splitPath(p), nil
}

// InvalidateHandle is a no-op: handles are deterministic, so there is nothing to
// forget.
func (h *handler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

// HandleLimit is effectively unbounded: handles are computed, not cached, so
// there is no table to cap and no eviction-induced staleness.
func (h *handler) HandleLimit() int { return 1 << 30 }

// clientKey reduces a remote address to a stable per-client identity (host
// without the ephemeral source port), so a client's several TCP connections
// count as one client.
func clientKey(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// joinAbs turns go-nfs path components into the index's "/"-rooted path.
func joinAbs(parts []string) string {
	if len(parts) == 0 {
		return "/"
	}
	return "/" + path.Join(parts...)
}

// splitPath turns a "/"-rooted path into go-nfs components ("/" -> nil).
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
