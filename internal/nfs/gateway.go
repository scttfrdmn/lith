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
	MarkDistinctRead(key string, off, length, size int64)
}

// Config configures the gateway.
type Config struct {
	Index      index.Reader
	Store      *blockstore.BlockStore
	RootID     [8]byte // first 8 bytes of the index file's sha256; the handle root id
	ClientIdle time.Duration
	// ReadIdle is how long a path counts as an active read stream after its last read, for
	// the readahead divisor (#337). See windowBlocks for why the divisor counts READ PATHS
	// rather than mounted clients, and readStreamIdleDefault for where this number comes
	// from and what does NOT depend on it.
	ReadIdle time.Duration
	Metrics  Metrics
	Logger   *slog.Logger
}

// Serve runs the gateway on ln until ctx is cancelled. ln carries both NFS and
// the embedded MOUNT service; clients mount with an explicit port (no portmap).
func Serve(ctx context.Context, ln net.Listener, cfg Config) error {
	h := newGateway(ctx, cfg)
	cfg = h.cfg
	// Route go-nfs's per-request trace into true per-op counters (#247) and its
	// own error/warn logs into lith's slog. SetLogger is a process global; one
	// gateway runs per `lith serve nfs` process.
	gonfs.SetLogger(newOpLogger(cfg.Logger, cfg.Metrics))
	go h.srv.reap(ctx)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	return gonfs.Serve(ln, h)
}

// newGateway builds the gateway's three pieces -- the shared server state, the read-only
// filesystem, and the go-nfs handler -- applies Config's defaults, and wires them together.
//
// IT EXISTS SO THERE IS ONE PLACE, which is not a style preference. The test harness built its
// own copy of this construction, and when #337 added `srv.readStreams = fs.activeReadStreams`
// here, the harness's copy did not get it -- so every gateway test kept exercising the
// MOUNT-counting fallback and TestFairShareArithmetic went on asserting the old divisor and
// passing. The fix was wired only in the function no test calls. Duplicated construction is
// how a change ships dead with a green suite, and this project has shipped one hook that way
// already (the ttfbRecorder in #341).
func newGateway(ctx context.Context, cfg Config) *handler {
	if cfg.ClientIdle <= 0 {
		cfg.ClientIdle = 5 * time.Minute
	}
	if cfg.ReadIdle <= 0 {
		cfg.ReadIdle = readStreamIdleDefault
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	srv := &server{cfg: cfg, clients: map[string]time.Time{}}
	fs := &roFS{cfg: cfg, srv: srv, ctx: ctx, states: map[string]*seqState{}}
	// The divisor's input (#337): paths READ recently, not clients MOUNTED recently.
	srv.readStreams = fs.activeReadStreams
	return &handler{cfg: cfg, srv: srv, fs: fs}
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
	// readStreams reports how many paths have been READ within cfg.ReadIdle -- the divisor
	// windowBlocks actually wants. Set once, when the roFS that owns the per-path state is
	// built; nil only in tests that construct a bare server, where windowBlocks falls back to
	// the client count and so reproduces the pre-#337 behaviour exactly.
	readStreams func() int
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

// readStreamIdleDefault is how long a path counts as an active read stream after its last
// read, for the readahead divisor (#337).
//
// WHAT DOES NOT DEPEND ON IT, which is the reason this number can be a judgement without being
// a risk: the reported defect is clients that MOUNT AND READ NOTHING, and such a client creates
// no per-path read state at all, so it leaves the divisor for ANY value of this. Measured
// externally, 47 of them cut one streaming client from ~1280 to ~540 MB/s -- 2.4x -- with
// BYTE-IDENTICAL transfers in every arm (3.777 GB, 458 GETs, prefetch issued == used), so it
// was purely window depth and purely from registration.
//
// What it does govern is a second, unmeasured population: a path a still-mounted client read a
// while ago and has not come back to. Today the only bound on those is the LRU cap on the state
// map (maxSeqStates = 8192), which over a long-lived gateway is a far worse divisor than the
// client count ever was -- so some recency window is required, not optional.
//
// 30 s, and the derivation is honest rather than device-derived. Too short and a client pausing
// between reads is dropped and re-granted repeatedly, which is the oscillation risk #312
// quantified on the FUSE side: the divisor merely DROPPING as handles closed produced eleven
// dispatch bursts in one 16-reader cell. Too long and this is the bug it replaces. The one real
// observation of HPC read cadence available is 5.0 s -- the ExtData time-slice gap measured on
// the reporting workload (#312) -- so 30 s is ~6x the longest pause a streaming client there
// takes, and 10x shorter than the 5-minute mount window that caused the collapse. Operators can
// move it with Config.ReadIdle.
//
// A DEVICE-DERIVED WINDOW WAS CONSIDERED AND REJECTED: the time to drain one window at the NIC
// rate is 0.04-0.4 s on a 15 Gbps box, which would shed a reader on any compute pause at all.
// There is no device quantity that yields seconds, and pretending one does would be the
// wrong-unit error that cost three releases on #340/#349.
const readStreamIdleDefault = 30 * time.Second

// windowBlocks is the per-read-stream readahead window: the mount-wide prefetch budget in
// blocks divided by the number of ACTIVE READ STREAMS, floored at 2 blocks so a single stream
// always gets some readahead and capped at 96.
//
// THE DIVISOR USED TO COUNT MOUNTED CLIENTS, AND THAT IS #301 IN THE GATEWAY (#337). A client
// that sent one MOUNT held a share of the budget for ClientIdle -- five minutes by default --
// whether or not it ever read a byte. Measured locally with NO client reading anything: 1 mount
// gave 96 blocks, 8 gave 61, 48 gave 10, and 246 reached the 2-block floor. A 48x collapse from
// registration alone.
//
// Measured externally on a real gateway with 47 namespaced clients that mounted and read
// nothing, against one streaming client: **1267-1300 MB/s with one client, 519-558 with 48 --
// 2.4x** -- and BYTE-IDENTICAL transfers in every arm (3.777 GB, 458 GETs, prefetch
// issued == used == 3594). So it was purely window depth and nothing was being wasted, which is
// the same byte-invisible signature #301 had and the reason that one took a dozen gates to
// find. Eight clients cost nothing; the reporter's own prediction of "under 20%" failed, and it
// binds at ~15 Gbps so a fatter NIC can only be worse -- and a gateway is the one place a fat
// NIC is the normal case, because it funnels a whole cluster through one.
//
// THE CORRECTED INPUT WAS ALWAYS AVAILABLE. roFS.states carries a per-path lastTouch that
// stateFor updates on every read. The comment at reap -- "per-op client activity is not visible
// through go-nfs's stateless read path" -- is true of go-nfs and NOT true of lith's own state,
// and that distinction is the whole bug. This is the gateway's analogue of #311's
// streamingHandles, and the same shape of one-line input change.
//
// WHY THIS IS SAFER THAN #309, THE ONE DIVISOR CHANGE THAT REGRESSED: a reader must have
// mounted, so active read streams can never exceed registered clients, so this change can only
// make a window LARGER. It cannot starve anyone -- and starvation is exactly what #309 did
// (byte-exact admission on the aggregate refused any handle that could not fit a full window,
// so "2 x 223 + 14 x 0" covered two readers where "16 x 30" covered sixteen, measured 11x
// slower). The risk here is the opposite one, over-commitment, and that is now bounded: the
// tier-pressure gate defaults ON for every export (#389), so the aggregate those larger windows
// commit is admitted against the memory tier rather than being unbounded. The two changes
// compose, and this one would have been materially riskier a week ago.
//
// Falls back to the client count when readStreams is unset, which only happens in tests that
// construct a bare server -- there it reproduces the pre-#337 behaviour exactly, which is what
// lets the characterization test assert both sides.
func (s *server) windowBlocks() int64 {
	budget := s.cfg.Store.PrefetchBudgetBlocks()
	if budget < 2 {
		budget = 2
	}
	divisor := s.activeClients()
	if s.readStreams != nil {
		divisor = s.readStreams()
	}
	w := budget / int64(divisor)
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
