package main

// The #256 rule, fit at the KEY level, and scored against the cold-start tax as well as
// follow-through.
//
// Why the row is a key and not a handle. The shared-cache pass (#274) found that 84-85% of
// all redeemed prefetch bytes are read by a DIFFERENT handle than the one that fetched
// them, on both mounts. A quantity five sixths of which belongs to somebody else is not a
// property of the handle, and neither is a predictor of it. If a predictor exists it is a
// property of the OBJECT's access pattern across all of its readers — which is what this
// scores.
//
// Two features here cannot exist at the handle level, and they are the reason to do this:
//
//	overlap_frac  1 - (union distinct bytes) / (sum of per-reader distinct bytes).
//	              0 = the readers partition the object; ->1 = they replicate each other.
//	chunk_overlap the same question one granularity up: the fraction of the 1 MiB cache
//	              chunks the key touches that are touched by MORE than one handle. Added
//	              after the first run, and labelled post-hoc for that reason, because
//	              overlap_frac came back a constant 0 on all 216 objects of both mounts —
//	              the readers partition the object BYTE for byte — while 47% of HEMCO's
//	              touched chunks are shared. That gap is the floor's mechanism in one line:
//	              the waste is not redundant reading, it is granularity.
//	interleave    the fraction of consecutive reads on the key that come from a different
//	              handle than the previous one. A measure of how badly concurrent readers
//	              scramble the object's access order — the thing that makes a per-handle
//	              detector see randomness where the object is being read in full.
//
// Two scoring targets, because upstream asked for the second and it is the one that is
// confirmed and actionable:
//
//	follow_through           did prefetch on this object pay off (shared unit)
//	cold_waste_per_distinct  the key's share of the ~1.23 GB cold-start granularity floor,
//	                         normalised by the bytes actually read from it. This is the
//	                         key-level analogue of the mount-level headline (HEMCO 33% vs
//	                         met 1.8%). The raw-byte version is reported too but is
//	                         scale-dependent by construction: a larger object can waste
//	                         more without being worse.
//
// FULL vs EARLY is the actionability split, and it is not cosmetic. A FULL-feature
// correlation is a description of the run. An EARLY-feature correlation — first k reads of
// the object in mount-wide `seq` order — is a candidate policy, because a granularity hint
// has to be decided from what is known when the object is first touched. Reported side by
// side so the difference between the two is visible rather than assumed.
//
// Accounting is NOT redefined here. Per-key dispatch, redemption and cold waste come from
// globalScore's own accumulators, attributed per key instead of per handle: same dedup,
// same EOF clamp, same first-run-cold rule. Only the read-side features are computed here.

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// keyAgg is the shared-cache accounting for one object, filled by globalScore.
type keyAgg struct {
	size              int64
	dispatchedBytes   int64
	usedBytes         int64
	crossHandleBytes  int64
	suppressedBlocks  int
	coldFirstRunReads int
	coldSuppressed    int
	coldNetWaste      int64
	coldGrossWaste    int64
}

// keyScore is one object's targets and the features offered a chance to predict them.
type keyScore struct {
	label, arm string
	key        string
	objSize    int64
	readers    int
	reads      int
	mismatches int // handles on this key whose replay diverged from the mount

	dispatchedBytes int64
	usedBytes       int64
	distinctBytes   int64
	followThrough   float64 // T1
	coldWasteBytes  int64   // T2
	coldPerDistinct float64 // T3
	coldFrac        float64 // waste / object size, reported not scored

	full  keyFeatures
	early keyFeatures
}

// keyFeatures is computed twice per key: over all reads, and over the first k.
type keyFeatures struct {
	readers          float64
	reads            float64
	readsPerReader   float64
	meanReadKiB      float64
	fracSmall        float64
	distinctFrac     float64
	spanFrac         float64
	overlapFrac      float64
	fracMonotonic    float64
	meanAbsGapBlocks float64
	interleave       float64
	fracStraddle     float64
	chunkOverlap     float64
	readersPerChunk  float64
	objSizeMiB       float64
}

type keyFeat struct {
	name string
	get  func(keyFeatures) float64
}

func keyFeatList() []keyFeat {
	return []keyFeat{
		{"readers", func(f keyFeatures) float64 { return f.readers }},
		{"reads_per_reader", func(f keyFeatures) float64 { return f.readsPerReader }},
		{"mean_read_kib", func(f keyFeatures) float64 { return f.meanReadKiB }},
		{"frac_small", func(f keyFeatures) float64 { return f.fracSmall }},
		{"distinct_frac", func(f keyFeatures) float64 { return f.distinctFrac }},
		{"span_frac", func(f keyFeatures) float64 { return f.spanFrac }},
		{"overlap_frac", func(f keyFeatures) float64 { return f.overlapFrac }},
		{"frac_monotonic", func(f keyFeatures) float64 { return f.fracMonotonic }},
		{"mean_abs_gap_blocks", func(f keyFeatures) float64 { return f.meanAbsGapBlocks }},
		{"interleave", func(f keyFeatures) float64 { return f.interleave }},
		{"frac_straddle", func(f keyFeatures) float64 { return f.fracStraddle }},
		// POST-HOC, added after seeing that overlap_frac is a constant 0 everywhere. Not
		// eligible for the pre-registered verdict; exploratory only.
		{"chunk_overlap*", func(f keyFeatures) float64 { return f.chunkOverlap }},
		{"readers_per_chunk*", func(f keyFeatures) float64 { return f.readersPerChunk }},
		// Also post-hoc, and the cheapest candidate there is: the object's SIZE, which the
		// mount knows at open() before a single read. Identical in the FULL and EARLY
		// columns because it does not depend on reads at all — which is exactly what makes
		// it implementable as a hint key.
		{"obj_size*", func(f keyFeatures) float64 { return f.objSizeMiB }},
	}
}

type keyTarget struct {
	name string
	get  func(keyScore) float64
	// binding says whether this target's verdict is the pre-registered one. The raw-byte
	// cold waste is scale-dependent, so it is printed and explicitly not counted.
	binding bool
	note    string
}

func keyTargets() []keyTarget {
	return []keyTarget{
		{"follow_through", func(s keyScore) float64 { return s.followThrough }, true, ""},
		{"cold_waste_bytes", func(s keyScore) float64 { return float64(s.coldWasteBytes) }, false,
			"scale-dependent by construction (a larger object can waste more); reported, NOT counted"},
		{"cold_waste_per_distinct", func(s keyScore) float64 { return s.coldPerDistinct }, true,
			"the key-level analogue of the mount headline (HEMCO 33% vs met 1.8%)"},
	}
}

// scoreKeys builds one row per object. rows must be the trace's rows; aggs must come from
// globalScore on the same trace, so that every byte quantity here is the shared-cache one.
func scoreKeys(label, arm string, cfg traceConfig, rows []row, perHandle []handleScore, aggs map[string]*keyAgg, k int, byteExact int64) []keyScore {
	ord := make([]row, len(rows))
	copy(ord, rows)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].seq < ord[j].seq })

	byKey := map[string][]row{}
	for _, r := range ord {
		if r.path != "window" {
			continue
		}
		byKey[r.key] = append(byKey[r.key], r)
	}

	// A key inherits the fidelity of its readers: if a handle reading this object replayed
	// a different program, the object's score is not evidence about lith either.
	badFH := map[uint64]bool{}
	for _, s := range perHandle {
		if s.mismatches > 0 {
			badFH[s.fh] = true
		}
	}

	out := make([]keyScore, 0, len(byKey))
	for key, rs := range byKey {
		a := aggs[key]
		if a == nil {
			a = &keyAgg{}
		}
		s := keyScore{label: label, arm: arm, key: key, objSize: a.size, reads: len(rs)}
		if s.objSize == 0 {
			s.objSize = keySize(rs)
		}
		seen := map[uint64]bool{}
		for _, r := range rs {
			if !seen[r.fh] {
				seen[r.fh] = true
				if badFH[r.fh] {
					s.mismatches++
				}
			}
		}
		s.readers = len(seen)
		s.distinctBytes = unionBytes(rs)
		s.dispatchedBytes = a.dispatchedBytes
		s.usedBytes = a.usedBytes
		s.coldWasteBytes = a.coldNetWaste
		if s.dispatchedBytes > 0 {
			s.followThrough = float64(s.usedBytes) / float64(s.dispatchedBytes)
		} else {
			// No fetch was charged to this object — every block a reader asked for was
			// already resident, or none was asked for. Nothing to score: NaN, not 0, so
			// the verdict excludes it rather than counting it as prefetch that failed.
			s.followThrough = math.NaN()
		}
		if s.distinctBytes > 0 {
			s.coldPerDistinct = float64(s.coldWasteBytes) / float64(s.distinctBytes)
		} else {
			s.coldPerDistinct = math.NaN()
		}
		if s.objSize > 0 {
			s.coldFrac = float64(s.coldWasteBytes) / float64(s.objSize)
		} else {
			s.coldFrac = math.NaN()
		}
		s.full = keyFeaturesOf(rs, s.objSize, cfg.blockSize, byteExact)
		n := k
		if n > len(rs) {
			n = len(rs)
		}
		s.early = keyFeaturesOf(rs[:n], s.objSize, cfg.blockSize, byteExact)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// keySize mirrors objSizeOf's rule at the key level: the recorded size if the trace has
// one, else the high-water mark, which under-counts rather than inflating.
func keySize(rs []row) int64 {
	var hw int64
	for _, r := range rs {
		if r.size > 0 {
			return r.size
		}
		if e := r.off + r.length; e > hw {
			hw = e
		}
	}
	return hw
}

func unionBytes(rs []row) int64 {
	type iv struct{ lo, hi int64 }
	ivs := make([]iv, 0, len(rs))
	for _, r := range rs {
		if r.length > 0 {
			ivs = append(ivs, iv{r.off, r.off + r.length})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo < ivs[j].lo })
	var total, curLo, curHi int64
	curLo, curHi = -1, -1
	for _, v := range ivs {
		if curHi < 0 {
			curLo, curHi = v.lo, v.hi
			continue
		}
		if v.lo > curHi {
			total += curHi - curLo
			curLo, curHi = v.lo, v.hi
			continue
		}
		if v.hi > curHi {
			curHi = v.hi
		}
	}
	if curHi >= 0 {
		total += curHi - curLo
	}
	return total
}

func keyFeaturesOf(rs []row, size, blockSize, byteExact int64) keyFeatures {
	var f keyFeatures
	if len(rs) == 0 {
		return f
	}
	f.reads = float64(len(rs))
	readers := map[uint64][]row{}
	var sumLen, small, straddle, mono, inter, gapSum float64
	var minOff, maxEnd int64
	minOff, maxEnd = math.MaxInt64, 0
	for i, r := range rs {
		readers[r.fh] = append(readers[r.fh], r)
		sumLen += float64(r.length)
		// The cold tax's own eligibility test, not a new threshold.
		if r.length <= byteExact && r.length < chunkSize {
			small++
		}
		if r.length > 0 && r.off/chunkSize != (r.off+r.length-1)/chunkSize {
			straddle++
		}
		if r.off < minOff {
			minOff = r.off
		}
		if e := r.off + r.length; e > maxEnd {
			maxEnd = e
		}
		if i == 0 {
			continue
		}
		p := rs[i-1]
		if r.off >= p.off {
			mono++
		}
		if r.fh != p.fh {
			inter++
		}
		if blockSize > 0 {
			d := r.off/blockSize - p.off/blockSize
			if d < 0 {
				d = -d
			}
			gapSum += float64(d)
		}
	}
	f.readers = float64(len(readers))
	f.readsPerReader = f.reads / f.readers
	f.meanReadKiB = sumLen / f.reads / 1024
	f.fracSmall = small / f.reads
	f.fracStraddle = straddle / f.reads
	if len(rs) > 1 {
		d := f.reads - 1
		f.fracMonotonic = mono / d
		f.interleave = inter / d
		f.meanAbsGapBlocks = gapSum / d
	} else {
		// One read: monotonicity and interleaving are undefined, not 1 and not 0. Saying
		// "perfectly sequential" about a single read is the kind of free-win-by-convention
		// that made the per-handle rule look predictive on handles that did nothing.
		f.fracMonotonic = math.NaN()
		f.interleave = math.NaN()
		f.meanAbsGapBlocks = math.NaN()
	}
	// Chunk-level sharing: who else touches the 1 MiB chunks this object's readers touch.
	// The byte-level answer (overlap_frac) is 0 on every object of both mounts, so the
	// only sharing that exists is at cache granularity — which is the quantity the cold
	// tax is made of.
	chunkReaders := map[int64]map[uint64]bool{}
	for _, r := range rs {
		if r.length <= 0 {
			continue
		}
		for c := r.off / chunkSize; c <= (r.off+r.length-1)/chunkSize; c++ {
			if chunkReaders[c] == nil {
				chunkReaders[c] = map[uint64]bool{}
			}
			chunkReaders[c][r.fh] = true
		}
	}
	if len(chunkReaders) > 0 {
		shared, sum := 0, 0
		for _, v := range chunkReaders {
			if len(v) > 1 {
				shared++
			}
			sum += len(v)
		}
		f.chunkOverlap = float64(shared) / float64(len(chunkReaders))
		f.readersPerChunk = float64(sum) / float64(len(chunkReaders))
	} else {
		f.chunkOverlap = math.NaN()
		f.readersPerChunk = math.NaN()
	}

	union := unionBytes(rs)
	var sumDistinct int64
	for _, rr := range readers {
		sumDistinct += unionBytes(rr)
	}
	if sumDistinct > 0 {
		f.overlapFrac = 1 - float64(union)/float64(sumDistinct)
	} else {
		f.overlapFrac = math.NaN()
	}
	if size > 0 {
		f.objSizeMiB = float64(size) / (1 << 20)
		f.distinctFrac = float64(union) / float64(size)
		f.spanFrac = float64(maxEnd-minOff) / float64(size)
	} else {
		f.objSizeMiB = math.NaN()
		f.distinctFrac = math.NaN()
		f.spanFrac = math.NaN()
	}
	return f
}

// reportKeys prints the per-trace key-level summary.
func reportKeys(ks []keyScore, blockSize int64) {
	var disp, used, cold, distinct int64
	scored, unfaithful := 0, 0
	for _, s := range ks {
		disp += s.dispatchedBytes
		used += s.usedBytes
		cold += s.coldWasteBytes
		distinct += s.distinctBytes
		if s.dispatchedBytes > 0 {
			scored++
		}
		if s.mismatches > 0 {
			unfaithful++
		}
	}
	fmt.Printf("   KEY level: %d objects, %d scoreable for follow-through, %d with a diverged reader\n",
		len(ks), scored, unfaithful)
	fmt.Printf("     dispatched %.1f MiB  followed through %.1f MiB (%.3f)   cold net waste %.1f MiB of %.1f MiB distinct (%.1f%%)\n",
		mibOf(disp), mibOf(used), ratioOf(used, disp), mibOf(cold), mibOf(distinct), 100*ratioOf(cold, distinct))
	reportConcentration(ks)
}

// reportConcentration answers the question that does not need a correlation: is the floor
// concentrated on a few objects? If it is, an object-scoped hint with a small budget
// recovers most of it even from a mediocre predictor.
func reportConcentration(ks []keyScore) {
	waste := make([]float64, 0, len(ks))
	var total float64
	for _, s := range ks {
		waste = append(waste, float64(s.coldWasteBytes))
		total += float64(s.coldWasteBytes)
	}
	if total == 0 {
		fmt.Printf("     cold waste is zero on every object: concentration undefined\n")
		return
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(waste)))
	share := func(frac float64) float64 {
		n := int(math.Ceil(frac * float64(len(waste))))
		var s float64
		for i := 0; i < n && i < len(waste); i++ {
			s += waste[i]
		}
		return s / total
	}
	half := 0
	var run float64
	for _, w := range waste {
		if run >= 0.5*total {
			break
		}
		run += w
		half++
	}
	fmt.Printf("     cold-waste concentration: top 1%% of keys hold %.1f%%, top 10%% hold %.1f%%, top 25%% hold %.1f%%;\n",
		100*share(0.01), 100*share(0.10), 100*share(0.25))
	fmt.Printf("       %d of %d objects (%.1f%%) hold half of it\n", half, len(waste), 100*float64(half)/float64(len(waste)))
}

// reportClassContrast prints each FULL feature's median per class. A within-class constant
// still carries information BETWEEN classes, and the correlation table cannot show it: a
// feature that is 1.000 on every HEMCO object and 0.2 on every met object predicts nothing
// about which HEMCO object pays the tax and everything about which mount does.
func reportClassContrast(all []keyScore, labels []string) {
	if len(labels) < 2 {
		return
	}
	med := func(xs []float64) float64 {
		if len(xs) == 0 {
			return math.NaN()
		}
		sort.Float64s(xs)
		return xs[len(xs)/2]
	}
	fmt.Printf("   per-class medians of the FULL features (what the within-class rho cannot show):\n")
	for _, ft := range keyFeatList() {
		line := fmt.Sprintf("     %-22s", ft.name)
		for _, lab := range labels {
			var xs []float64
			for _, s := range all {
				if s.label == lab {
					if v := ft.get(s.full); !math.IsNaN(v) {
						xs = append(xs, v)
					}
				}
			}
			line += fmt.Sprintf("  %s=%.3f", lab, med(xs))
		}
		fmt.Println(line)
	}
}

// reportKeyVerdict fits every feature against every target, FULL and EARLY, under the same
// rule the per-handle verdict uses (upstream's spearman, degenerate gate, min-n, AUC).
func reportKeyVerdict(all []keyScore, labels []string, minN, k int) {
	fmt.Printf("\n== KEY-LEVEL verdict (#256 at the object level)\n")
	faithful := make([]keyScore, 0, len(all))
	dropped := 0
	for _, s := range all {
		if s.mismatches == 0 {
			faithful = append(faithful, s)
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		fmt.Printf("   fidelity filter: %d objects excluded because a handle reading them diverged from the\n", dropped)
		fmt.Printf("     mount. A key inherits its readers' fidelity; an object read by a handle replaying a\n")
		fmt.Printf("     different program is not evidence about lith.\n")
	}
	all = faithful

	arms := map[string][]keyScore{}
	armOrder := []string{}
	for _, s := range all {
		nm := s.label
		if s.arm != "" {
			nm = s.label + "/" + s.arm
		}
		if _, ok := arms[nm]; !ok {
			armOrder = append(armOrder, nm)
		}
		arms[nm] = append(arms[nm], s)
	}
	sort.Strings(armOrder)
	fmt.Printf("   objects per arm:")
	for _, nm := range armOrder {
		fmt.Printf("  %s=%d", nm, len(arms[nm]))
	}
	fmt.Printf("   (--min-n=%d, minDistinct=%d apply to OBJECTS here, not handles)\n", minN, minDistinct)
	reportClassContrast(all, labels)

	for _, tg := range keyTargets() {
		fmt.Printf("\n   -- target: %s", tg.name)
		if !tg.binding {
			fmt.Printf("   [NOT COUNTED: %s]", tg.note)
		} else if tg.note != "" {
			fmt.Printf("   (%s)", tg.note)
		}
		fmt.Println()
		if lab := labels; len(lab) == 2 {
			var a, b []float64
			for _, s := range all {
				v := tg.get(s)
				if math.IsNaN(v) {
					continue
				}
				if s.label == lab[0] {
					a = append(a, v)
				} else if s.label == lab[1] {
					b = append(b, v)
				}
			}
			if v, ok := aucMannWhitney(a, b); ok {
				fmt.Printf("      AUC(%s vs %s) = %.3f  (n=%d vs %d)\n", lab[0], lab[1], math.Max(v, 1-v), len(a), len(b))
			}
		}
		bestFull, bestFullName := 0.0, "(none)"
		bestEarly, bestEarlyName := 0.0, "(none)"
		fmt.Printf("      %-22s %-28s %s\n", "feature", "FULL (all reads)", fmt.Sprintf("EARLY (first %d reads of the key)", k))
		for _, ft := range keyFeatList() {
			fullLine, earlyLine := "", ""
			fullQual, earlyQual := 0, 0
			for _, nm := range armOrder {
				// Each fit gets its OWN filter. A shared one would drop every object whose
				// EARLY value is undefined from the FULL fit too, silently changing the
				// FULL population to make the two columns comparable — the same
				// population-vs-unit confound the shared-cache flip had to be controlled
				// for, reintroduced one level down.
				var xf, yf, xe, ye []float64
				for _, s := range arms[nm] {
					y := tg.get(s)
					if math.IsNaN(y) {
						continue
					}
					if v := ft.get(s.full); !math.IsNaN(v) {
						xf = append(xf, v)
						yf = append(yf, y)
					}
					if v := ft.get(s.early); !math.IsNaN(v) {
						xe = append(xe, v)
						ye = append(ye, y)
					}
				}
				postHoc := strings.HasSuffix(ft.name, "*")
				fullLine += fitCell(nm, xf, yf, minN, postHoc, &fullQual, &bestFull, &bestFullName, ft.name)
				earlyLine += fitCell(nm, xe, ye, minN, postHoc, &earlyQual, &bestEarly, &bestEarlyName, ft.name)
			}
			mark := ""
			if fullQual >= 2 {
				mark += " <-FULL"
			}
			if earlyQual >= 2 {
				mark += " <-EARLY"
			}
			fmt.Printf("      %-22s %-28s %s%s\n", ft.name, fullLine, earlyLine, mark)
		}
		fmt.Printf("      best |rho|: FULL %.3f (%s)   EARLY %.3f (%s)", bestFull, bestFullName, bestEarly, bestEarlyName)
		if bestFull > 0 {
			fmt.Printf("   EARLY/FULL = %.2f", bestEarly/bestFull)
		}
		fmt.Println()
	}
	reportRecovery(arms, armOrder, k)
}

// fitCell is one arm's cell: the rho, or the reason it does not count. Only values that
// pass upstream's degenerate gate are allowed to move `best`, so a verdict cannot be built
// out of n=3 monotone-by-construction fits.
//
// It never prints a bare "n/a". A feature that is CONSTANT across every object of a class
// cannot predict within that class, but its constancy is a fact about the workload and may
// be the very thing that separates the classes — frac_small is 1.000 on all 204 HEMCO
// objects, which is not a missing measurement, it is why the tax lands there. An
// unexplained n/a reads as the former when it is the latter.
func fitCell(name string, xs, ys []float64, minN int, postHoc bool, qual *int, best *float64, bestName *string, feature string) string {
	r, ok := spearman(xs, ys)
	if !ok {
		switch {
		case len(xs) < 3:
			return fmt.Sprintf(" %s=n/a[n=%d]", name, len(xs))
		case distinct(xs) == 1:
			return fmt.Sprintf(" %s=const(%g)", name, xs[0])
		case distinct(ys) == 1:
			return fmt.Sprintf(" %s=n/a[target const]", name)
		default:
			return fmt.Sprintf(" %s=n/a", name)
		}
	}
	if why := degenerate(xs, ys, minN); why != "" {
		return fmt.Sprintf(" %s=%+.3f[%s]", name, r, why)
	}
	if postHoc {
		// Exploratory: printed, but it may not qualify a feature or set the best |rho| the
		// pre-registered verdict is read off. A feature invented after seeing the answers
		// is a hypothesis for the next run, not evidence from this one.
		return fmt.Sprintf(" %s=%+.3f", name, r)
	}
	if math.Abs(r) >= 0.5 {
		*qual++
	}
	if math.Abs(r) > *best {
		*best, *bestName = math.Abs(r), feature
	}
	return fmt.Sprintf(" %s=%+.3f", name, r)
}

// reportRecovery restates the fit in the units of the fix. Suppose a granularity hint can
// be attached to a budget of objects. Rank objects by an EARLY feature, hint the top N%,
// and count how much of the arm's actual cold waste those objects carry — against an ORACLE
// that ranks by the waste itself. A rho says whether a signal exists; this says how much of
// the floor it would recover, which is what an implementer has to decide on.
func reportRecovery(arms map[string][]keyScore, armOrder []string, k int) {
	fmt.Printf("\n   -- recovery at budget: rank objects by an EARLY feature, hint the top N%%, sum their\n")
	fmt.Printf("      ACTUAL cold waste. ORACLE ranks by the waste itself (the ceiling). Random = the budget.\n")
	budgets := []float64{0.05, 0.10, 0.25}
	for _, nm := range armOrder {
		ks := arms[nm]
		var total float64
		for _, s := range ks {
			total += float64(s.coldWasteBytes)
		}
		if total == 0 {
			fmt.Printf("      %-10s no cold waste on any object: recovery undefined\n", nm)
			continue
		}
		recov := func(rank func(keyScore) float64, frac float64) float64 {
			ordered := make([]keyScore, len(ks))
			copy(ordered, ks)
			sort.SliceStable(ordered, func(i, j int) bool {
				a, b := rank(ordered[i]), rank(ordered[j])
				if math.IsNaN(a) {
					return false
				}
				if math.IsNaN(b) {
					return true
				}
				return a > b
			})
			n := int(math.Ceil(frac * float64(len(ordered))))
			var s float64
			for i := 0; i < n && i < len(ordered); i++ {
				s += float64(ordered[i].coldWasteBytes)
			}
			return s / total
		}
		fmt.Printf("      %s (n=%d objects, %.1f MiB of waste)\n", nm, len(ks), mibOf(int64(total)))
		for _, b := range budgets {
			or := recov(func(s keyScore) float64 { return float64(s.coldWasteBytes) }, b)
			line := fmt.Sprintf("        top %4.0f%%: oracle %5.1f%%", 100*b, 100*or)
			for _, ft := range keyFeatList() {
				got := recov(func(s keyScore) float64 { return ft.get(s.early) }, b)
				// Also try the descending order of the same feature: a predictor is
				// allowed to point either way, and which way is an empirical question.
				inv := recov(func(s keyScore) float64 { return -ft.get(s.early) }, b)
				if inv > got {
					got = inv
				}
				if or > 0 && got/or >= 0.5 {
					line += fmt.Sprintf("   %s %.1f%% (%.2f of oracle)", ft.name, 100*got, got/or)
				}
			}
			fmt.Println(line)
		}
	}
	fmt.Printf("      (features listed only when they reach >= 0.50 of the oracle at that budget; the better of\n")
	fmt.Printf("       the feature's two orderings is used, since a predictor may point either way.)\n")
}

func writeKeyCSV(path string, all []keyScore) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	hdr := []string{"label", "arm", "key", "obj_size", "readers", "reads", "reader_mismatches",
		"dispatched_bytes", "used_bytes", "distinct_bytes", "follow_through",
		"cold_waste_bytes", "cold_waste_per_distinct", "cold_waste_frac_obj"}
	for _, ft := range keyFeatList() {
		hdr = append(hdr, "full_"+ft.name)
	}
	for _, ft := range keyFeatList() {
		hdr = append(hdr, "early_"+ft.name)
	}
	if err := w.Write(hdr); err != nil {
		return err
	}
	ff := func(v float64) string {
		if math.IsNaN(v) {
			return ""
		}
		return strconv.FormatFloat(v, 'g', 6, 64)
	}
	for _, s := range all {
		rec := []string{s.label, s.arm, s.key,
			strconv.FormatInt(s.objSize, 10), strconv.Itoa(s.readers), strconv.Itoa(s.reads),
			strconv.Itoa(s.mismatches),
			strconv.FormatInt(s.dispatchedBytes, 10), strconv.FormatInt(s.usedBytes, 10),
			strconv.FormatInt(s.distinctBytes, 10), ff(s.followThrough),
			strconv.FormatInt(s.coldWasteBytes, 10), ff(s.coldPerDistinct), ff(s.coldFrac)}
		for _, ft := range keyFeatList() {
			rec = append(rec, ff(ft.get(s.full)))
		}
		for _, ft := range keyFeatList() {
			rec = append(rec, ff(ft.get(s.early)))
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
