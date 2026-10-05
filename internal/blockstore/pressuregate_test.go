// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// pressureRec counts the gate's holds alongside the eviction signal.
type pressureRec struct {
	budgetRec
	held atomic.Int64
}

func (r *pressureRec) PrefetchPressureHeld() { r.held.Add(1) }

// runPressureCell drives N readers over N distinct objects with every reader advancing the
// whole budget ahead — the overcommit shape #313 reports, where the divisor has not yet
// risen because no stream is established. Returns the eviction and hold counts and the peak
// resident-unread fraction.
func runPressureCell(t *testing.T, readers, chunksPerObj int, tier int64, pressureMax float64) (evicted, held int64, peakPressure float64) {
	t.Helper()
	const blockSize = int64(1) << 20
	srv := fake.New()
	keys := make([]Key, readers)
	for i := range readers {
		name := "obj" + strconv.Itoa(i)
		makeObj(srv, name, chunksPerObj)
		keys[i] = keyFor(t, srv, name)
	}
	rec := &pressureRec{}
	bs := newStore(t, srv, Config{
		BlockSize: blockSize, MemCache: tier, PrefetchBudget: tier / 2,
		PrefetchPressureMax: pressureMax, Recorder: rec,
	})

	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				p := bs.PrefetchPressure()
				for {
					old := peak.Load()
					if p <= float64FromBits(old) || peak.CompareAndSwap(old, float64Bits(p)) {
						break
					}
				}
			}
		}
	}()
	runReaders(t, bs, keys, chunksPerObj, blockSize, bs.PrefetchBudgetBlocks())
	close(stop)
	<-done
	return rec.evicted.Load(), rec.held.Load(), float64FromBits(peak.Load())
}

// #313: the pressure gate must cut eviction-before-read, bound the quantity it reads, and
// be INERT when off.
//
// What this fixture can and cannot show. The fake server returns instantly, so dispatch
// outruns consumption in a way a network cannot, and eviction-before-read here is a GRADIENT
// rather than the clean separation real S3 produces (externally: 0 evictions clean, 19-25k
// collapsed). So this asserts a CONTRAST between gate-off and gate-on at one sizing, never an
// absolute count -- the same reason TestPrefetchDivisorEarnsItsCost is a contrast, and
// bench/prefetch-divisor carries the real-S3 figures.
//
// The threshold swept on this fixture, for the record. Each cell is one run, so treat the
// counts as the shape and not as figures to quote:
//
//	threshold   evicted off -> on   peak pressure off -> on
//	0.5             227 -> 18            8.000 -> 0.562
//	0.85            265 -> 45            8.000 -> 0.938
//	1.0             252 -> 55            8.000 -> 1.062
//
// A resident-unread gate at 0.85, for comparison, managed 204 -> 186 and left peak pressure
// at 1.000 -- it did not bound its own quantity. That is why the gate reads committed.
func TestPressureGateCutsEvictionBeforeRead(t *testing.T) {
	const readers, chunksPerObj = 16, 24
	const tier = int64(16) << 20
	const threshold = 0.85

	offEvicted, offHeld, offPeak := runPressureCell(t, readers, chunksPerObj, tier, 0)
	onEvicted, onHeld, onPeak := runPressureCell(t, readers, chunksPerObj, tier, threshold)
	t.Logf("gate off: evicted=%d held=%d peakPressure=%.3f", offEvicted, offHeld, offPeak)
	t.Logf("gate on:  evicted=%d held=%d peakPressure=%.3f", onEvicted, onHeld, onPeak)

	// FIXTURE PRECONDITIONS. The off arm must actually overcommit and must actually evict
	// unread bytes, or the on arm has nothing to improve and a pass means nothing.
	if offPeak < 2.0 {
		t.Fatalf("fixture: gate-off peak pressure %.3f -- this cell does not overcommit "+
			"the tier, so it does not reproduce eviction-before-read", offPeak)
	}
	if offEvicted == 0 {
		t.Fatalf("fixture: gate-off evicted nothing unread, so there is no collapse here")
	}

	// INERT WHEN OFF. A zero threshold must not hold a single dispatch; this is what makes
	// shipping it off-by-default meaningful rather than nominal.
	if offHeld != 0 {
		t.Errorf("the gate held %d dispatches with PrefetchPressureMax=0", offHeld)
	}

	// IT MUST FIRE.
	if onHeld == 0 {
		t.Fatal("the gate never fired even though the off arm overcommitted the tier " +
			"several times over -- it is wired but not reachable")
	}

	// IT MUST BOUND THE QUANTITY IT READS. This is the assertion the resident-unread
	// version failed, and failing it is what sent the gate to committed: a gate whose own
	// signal keeps climbing is not admitting against anything.
	//
	// DIRECTION, NOT MAGNITUDE, and the first version of this got that wrong. It asserted
	// a halving, which held in isolation (8.000 -> 0.562/0.938) and FLAKED in the full-tree
	// run at 8.000 -> 4.750, because how much the gate buys depends on how dispatches
	// interleave with consumption and the whole suite running in parallel changes that.
	// A reduction is the property; its size is scheduling. The magnitudes in the table
	// above come from isolated runs and are not asserted anywhere.
	//
	// Still non-vacuous against the variant that sent the gate here: resident-unread gives
	// 1.000 against 1.000, which is not a reduction.
	if onPeak >= offPeak {
		t.Errorf("peak pressure %.3f with the gate on against %.3f off: it is not "+
			"bounding the quantity it admits against", onPeak, offPeak)
	}

	// AND IT MUST BUY SOMETHING. The byte that matters is eviction-before-read, since that
	// is what returns as a synchronous demand read and what the fairness collapse is made
	// of.
	if onEvicted >= offEvicted {
		t.Errorf("evicted-unread %d with the gate on vs %d off: the gate fired %d times "+
			"and bought nothing", onEvicted, offEvicted, onHeld)
	}
}

// The gate must not fire on a workload that was never under pressure — otherwise it is a
// window cap by another name, and a static cap is what cost 2.56x cross-region.
func TestPressureGateDoesNotFireWithRoomInTheTier(t *testing.T) {
	// Two readers against a tier eight times the earlier cell's, so resident-unread stays
	// far below capacity.
	evicted, held, peak := runPressureCell(t, 2, 24, int64(128)<<20, 0.85)
	t.Logf("2 readers, 128 MiB tier: evicted=%d held=%d peakPressure=%.3f", evicted, held, peak)
	if peak >= 0.85 {
		t.Skipf("fixture: peak pressure %.3f reached the threshold, so this cell cannot "+
			"show the gate staying out of the way", peak)
	}
	if held != 0 {
		t.Errorf("the gate held %d dispatches at peak pressure %.3f, below its 0.85 "+
			"threshold", held, peak)
	}
}

// Pressure is 0 — and the gate therefore inert — when there is no memory tier, rather than
// dividing by a zero capacity.
func TestPressureIsZeroWithoutATier(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	bs, err := New(srv, Config{Bucket: "bkt", BlockSize: 1 << 20, MemCache: 0,
		MaxRange: 64 << 20, PrefetchPressureMax: 0.85})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(bs.Close)
	if p := bs.PrefetchPressure(); p != 0 {
		t.Errorf("pressure %v with no tier, want 0", p)
	}
	// And a dispatch must still work rather than being gated by a meaningless ratio.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); bs.Prefetch(t.Context(), k, 0, 4*mib) }()
	wg.Wait()
}

func float64Bits(f float64) uint64     { return math.Float64bits(f) }
func float64FromBits(u uint64) float64 { return math.Float64frombits(u) }
