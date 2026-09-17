// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net"
	"path"
	"strings"
	"sync/atomic"
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

	// STALE/miss diagnostics (#244): a prefix-scoped read-only gateway that
	// returns NFS3ERR_STALE should say *why* — the reason a black-box "the server
	// chose STALE" cost the GCHP tester two probes. Counted by category and logged
	// on powers of two so a high stale rate cannot flood the log (lock-free).
	staleBadLen   atomic.Uint64 // handle not 16 bytes (malformed / truncated)
	staleRootID   atomic.Uint64 // handle from a different index (rebuilt/rebuilt root id)
	staleUnknIno  atomic.Uint64 // valid root id, inode no entry owns
	toHandleEmpty atomic.Uint64 // ToHandle could not resolve a path → unusable handle
}

var _ gonfs.Handler = (*handler)(nil)

// logStaleEvery logs on counts 1,2,4,8,… — exponential backoff so a 17% stale
// rate over millions of ops still yields a handful of lines, no lock, no time.
func logStaleEvery(c *atomic.Uint64, log *slog.Logger, msg string, attrs ...any) {
	n := c.Add(1)
	if n&(n-1) != 0 { // not a power of two
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn(msg, append([]any{"count", n}, attrs...)...)
}

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
		// An unresolvable path yields an empty handle, which every later op then
		// STALEs on. Logged (#244) because if this ever fires under load it is the
		// upstream cause of a burst of GETATTR stales.
		logStaleEvery(&h.toHandleEmpty, h.cfg.Logger, "nfs: ToHandle could not resolve path (returning empty handle)",
			"path", joinAbs(pathparts))
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
	stale := &gonfs.NFSStatusError{NFSStatus: gonfs.NFSStatusStale}
	if len(fh) != 16 {
		logStaleEvery(&h.staleBadLen, h.cfg.Logger, "nfs: STALE — handle is not 16 bytes (malformed/truncated)",
			"len", len(fh), "handle", hex.EncodeToString(fh))
		return nil, nil, stale
	}
	if !bytes.Equal(fh[0:8], h.cfg.RootID[:]) {
		logStaleEvery(&h.staleRootID, h.cfg.Logger, "nfs: STALE — handle root id does not match this index (rebuilt/different index?)",
			"handle", hex.EncodeToString(fh))
		return nil, nil, stale
	}
	ino := binary.BigEndian.Uint64(fh[8:16])
	p, ok := h.cfg.Index.ByInode(ino)
	if !ok {
		logStaleEvery(&h.staleUnknIno, h.cfg.Logger, "nfs: STALE — inode not found in index",
			"inode", ino, "handle", hex.EncodeToString(fh))
		return nil, nil, stale
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
