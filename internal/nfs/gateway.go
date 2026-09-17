// SPDX-License-Identifier: Apache-2.0

// Package nfs implements lith's read-only NFSv3 gateway (#143/#144): one node
// holds the Index and BlockStore and serves N clients over NFSv3, so a cluster
// pays one node's S3 traffic. It builds on willscott/go-nfs for the protocol and
// MOUNT, replacing go-nfs's ephemeral CachingHandler with deterministic
// (root id, inode) handles so a handle survives a gateway restart against the
// same index and returns NFS3ERR_STALE against a rebuilt one.
package nfs

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	gonfs "github.com/willscott/go-nfs"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
)

// Metrics is the optional metrics sink (satisfied by *metrics.Metrics). Nil-safe
// at the call sites.
type Metrics interface {
	NFSClients(delta int)
	NFSSeqStates(n int)
	NFSOp(op string)
	NFSReadBytes(n int64)
}

// Config configures the gateway.
type Config struct {
	Index      index.Reader
	Store      *blockstore.BlockStore
	RootID     [8]byte // first 8 bytes of the index file's sha256; the handle root id
	ClientIdle time.Duration
	Metrics    Metrics
	Logger     *slog.Logger
}

// Serve runs the gateway on ln until ctx is cancelled. ln carries both NFS and
// the embedded MOUNT service; clients mount with an explicit port (no portmap).
func Serve(ctx context.Context, ln net.Listener, cfg Config) error {
	if cfg.ClientIdle <= 0 {
		cfg.ClientIdle = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	srv := &server{cfg: cfg, clients: map[string]time.Time{}}
	fs := &roFS{cfg: cfg, srv: srv, ctx: ctx, states: map[string]*seqState{}}
	h := &handler{cfg: cfg, srv: srv, fs: fs}
	// Route go-nfs's per-request trace into true per-op counters (#247) and its
	// own error/warn logs into lith's slog. SetLogger is a process global; one
	// gateway runs per `lith serve nfs` process.
	gonfs.SetLogger(newOpLogger(cfg.Logger, cfg.Metrics))
	go srv.reap(ctx)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	return gonfs.Serve(ln, h)
}

// server holds the shared gateway state: the client registry (for the readahead
// window and the clients gauge) and the derived readahead window. The registry
// tracks MOUNT registrations, not per-operation activity — go-nfs's stateless
// read path does not carry a client identity — so the window below is a
// mount-registration-based share of the global budget, not a measured per-client
// fair share (#197).
type server struct {
	cfg     Config
	mu      sync.Mutex
	clients map[string]time.Time // remote addr -> last MOUNT time
}

func (s *server) mountClient(addr string) {
	s.mu.Lock()
	_, existed := s.clients[addr]
	s.clients[addr] = time.Now()
	n := len(s.clients)
	s.mu.Unlock()
	if !existed && s.cfg.Metrics != nil {
		s.cfg.Metrics.NFSClients(1)
	}
	_ = n
}

// reap expires clients idle since their last MOUNT beyond ClientIdle, returning
// their share of the readahead budget. (Per-op client activity is not visible
// through go-nfs's stateless read path — see the package/PR notes — so idle is
// measured from MOUNT.)
func (s *server) reap(ctx context.Context) {
	t := time.NewTicker(s.cfg.ClientIdle / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-s.cfg.ClientIdle)
			s.mu.Lock()
			var dropped int
			for a, seen := range s.clients {
				if seen.Before(cutoff) {
					delete(s.clients, a)
					dropped++
				}
			}
			s.mu.Unlock()
			if dropped > 0 && s.cfg.Metrics != nil {
				s.cfg.Metrics.NFSClients(-dropped)
			}
		}
	}
}

func (s *server) activeClients() int {
	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n < 1 {
		return 1
	}
	return n
}

// windowBlocks is the per-read-stream readahead window: the mount-wide prefetch
// budget in blocks divided by the count of currently-registered (mounted,
// not-idle) clients, floored at 2 blocks so a single stream always gets some
// readahead. One client alone gets the whole budget; N registered clients each
// get 1/N. This bounds total in-flight prefetch by the global budget; it is a
// registration-based share, not a measured per-client fair share (a client
// streaming after its idle expiry is absent from the denominator).
func (s *server) windowBlocks() int64 {
	budget := s.cfg.Store.PrefetchBudgetBlocks()
	if budget < 2 {
		budget = 2
	}
	w := budget / int64(s.activeClients())
	if w < 2 {
		w = 2
	}
	// Cap the readahead window: past a few hundred MB in flight, deeper dispatch
	// only queues goroutines behind the block store's prefetch semaphore without
	// adding throughput. 96 blocks ≈ 768 MB at an 8 MiB block.
	if w > 96 {
		w = 96
	}
	return w
}
