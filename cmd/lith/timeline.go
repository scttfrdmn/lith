// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// mountTimeline is a diagnostic blockstore.Recorder that captures a per-chunk
// demand-read timeline plus each prefetch's dispatch time, so the #70 join-wait
// question can be answered: is the app catching the prefetch frontier (join
// waits ≈ one round-trip) or is demand-fill TTFB the floor? It implements the
// base Recorder (tallies) and the optional chunkTimelineRecorder extension
// (PrefetchDispatch/ChunkRead). Installed only when `lith mount --timeline-csv`
// is set; otherwise the read path runs unaffected.
type mountTimeline struct {
	t0 time.Time

	mu       sync.Mutex
	dispatch map[string]time.Time // "key\x00chunk" -> first prefetch dispatch ts
	rows     []chunkRow

	memHit, diskHit, miss, uncovered atomic.Int64
	pfIssued, pfHit                  atomic.Int64
}

type chunkRow struct {
	ms         float64 // ms since mount start at read entry
	key        string
	chunk      int64
	kind       string // hit | join | uncovered
	prefetched bool
	waitMs     float64 // read entry -> data ready (join wait, or self-fill for uncovered)
	inflight   int     // chunk fetches in flight at read entry
	lagMs      float64 // prefetch dispatch -> read entry; -1 if never prefetched
}

func newMountTimeline() *mountTimeline {
	return &mountTimeline{t0: time.Now(), dispatch: make(map[string]time.Time)}
}

// base Recorder (tallies only).
func (m *mountTimeline) MemHit()           { m.memHit.Add(1) }
func (m *mountTimeline) DiskHit()          { m.diskHit.Add(1) }
func (m *mountTimeline) Miss()             { m.miss.Add(1) }
func (m *mountTimeline) UncoveredMiss()    { m.uncovered.Add(1) }
func (m *mountTimeline) PrefetchIssued()   { m.pfIssued.Add(1) }
func (m *mountTimeline) PrefetchHit()      { m.pfHit.Add(1) }
func (m *mountTimeline) S3Get(int64, bool) {}
func (m *mountTimeline) StartInflight()    {}
func (m *mountTimeline) EndInflight()      {}
func (m *mountTimeline) StaleKey(string)   {}

// chunkTimelineRecorder extension.
func (m *mountTimeline) PrefetchDispatch(key string, chunk int64, at time.Time) {
	dk := dispatchKey(key, chunk)
	m.mu.Lock()
	if _, ok := m.dispatch[dk]; !ok {
		m.dispatch[dk] = at // first dispatch wins (the lead the app races)
	}
	m.mu.Unlock()
}

func (m *mountTimeline) ChunkRead(ev blockstore.ChunkEvent) {
	lag := -1.0
	dk := dispatchKey(ev.Key, ev.Chunk)
	m.mu.Lock()
	if d, ok := m.dispatch[dk]; ok {
		lag = float64(ev.At.Sub(d).Microseconds()) / 1000
	}
	m.rows = append(m.rows, chunkRow{
		ms:         float64(ev.At.Sub(m.t0).Microseconds()) / 1000,
		key:        ev.Key,
		chunk:      ev.Chunk,
		kind:       ev.Kind,
		prefetched: ev.Prefetched,
		waitMs:     float64(ev.Wait.Microseconds()) / 1000,
		inflight:   ev.Inflight,
		lagMs:      lag,
	})
	m.mu.Unlock()
}

func dispatchKey(key string, chunk int64) string {
	return key + "\x00" + fmt.Sprint(chunk)
}

// writeCSV writes the per-chunk timeline. Returns the row count.
func (m *mountTimeline) writeCSV(path string) (int, error) {
	m.mu.Lock()
	rows := make([]chunkRow, len(m.rows))
	copy(rows, m.rows)
	m.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ms < rows[j].ms })

	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"ms", "kind", "prefetched", "wait_ms", "inflight", "lag_ms", "chunk", "key"}); err != nil {
		return 0, err
	}
	for _, r := range rows {
		rec := []string{
			strconv.FormatFloat(r.ms, 'f', 3, 64),
			sanitizeCSVCell(r.kind),
			strconv.FormatBool(r.prefetched),
			strconv.FormatFloat(r.waitMs, 'f', 3, 64),
			strconv.Itoa(r.inflight),
			strconv.FormatFloat(r.lagMs, 'f', 3, 64),
			strconv.FormatInt(r.chunk, 10),
			sanitizeCSVCell(r.key),
		}
		if err := w.Write(rec); err != nil {
			return 0, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// sanitizeCSVCell neutralizes spreadsheet formula injection: encoding/csv
// already quotes/escapes structural characters (comma, quote, newline), but a
// cell whose first character is one of = + - @ \t \r is interpreted as a live
// formula when the file is opened in Excel/Sheets. Per OWASP guidance we prefix
// such a cell with a single quote to force it to be treated as literal text.
func sanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// summary returns a short human-readable digest of the distributions #70 asks
// for: join-wait quantiles, dispatch->open lag quantiles, and peak in-flight.
func (m *mountTimeline) summary() string {
	m.mu.Lock()
	rows := make([]chunkRow, len(m.rows))
	copy(rows, m.rows)
	m.mu.Unlock()

	var joins, uncovered, hits []float64
	var lags []float64
	peakInflight := 0
	kindN := map[string]int{}
	for _, r := range rows {
		kindN[r.kind]++
		if r.inflight > peakInflight {
			peakInflight = r.inflight
		}
		switch r.kind {
		case "join":
			joins = append(joins, r.waitMs)
		case "uncovered":
			uncovered = append(uncovered, r.waitMs)
		case "hit":
			hits = append(hits, r.waitMs)
		}
		if r.lagMs >= 0 {
			lags = append(lags, r.lagMs)
		}
	}

	b := &fmtBuilder{}
	b.line("reads=%d  hit=%d join=%d uncovered=%d  prefetch issued=%d used=%d",
		len(rows), kindN["hit"], kindN["join"], kindN["uncovered"],
		m.pfIssued.Load(), m.pfHit.Load())
	b.line("peak in-flight fills=%d", peakInflight)
	b.dist("join_wait ms", joins)
	b.dist("uncovered self-fill ms", uncovered)
	b.dist("hit serve ms (sanity ~0)", hits)
	b.dist("dispatch->open lag ms (covered reads)", lags)
	return b.String()
}

type fmtBuilder struct{ s string }

func (b *fmtBuilder) line(f string, a ...any) { b.s += fmt.Sprintf(f, a...) + "\n" }
func (b *fmtBuilder) String() string          { return b.s }

func (b *fmtBuilder) dist(label string, xs []float64) {
	if len(xs) == 0 {
		b.line("%s: (none)", label)
		return
	}
	sort.Float64s(xs)
	q := func(p float64) float64 {
		i := int(p * float64(len(xs)-1))
		return xs[i]
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	b.line("%s: n=%d min=%.1f p50=%.1f p90=%.1f p99=%.1f max=%.1f mean=%.1f",
		label, len(xs), xs[0], q(0.5), q(0.9), q(0.99), xs[len(xs)-1], sum/float64(len(xs)))
}
