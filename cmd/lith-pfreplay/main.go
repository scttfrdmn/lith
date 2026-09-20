// SPDX-License-Identifier: Apache-2.0

// Command lith-pfreplay replays a `lith mount --pf-trace` file through the real
// access-pattern detector and scores, per file handle, what fraction of the BYTES
// it caused to be prefetched were subsequently read by that same handle.
//
// It exists to answer one question offline that four hypotheses answered wrongly on
// the cluster (#256): does anything observable in a handle's early reads predict
// whether prefetching for it will pay off? Each of those four cost a ~35-minute
// 48-rank job to refute. A replay costs a laptop second.
//
// The scoring rule was agreed with the reporting workload's maintainer and fixed
// BEFORE either side had data, so it cannot be tuned to the answer:
//
//   - Score BYTES, not chunk touches. lith's own lith_prefetch_used_total counts a
//     1 MiB chunk as used when a 64 KiB read touches it, which overstates
//     follow-through most for exactly the scattered readers under investigation.
//   - SEPARATION exists iff some feature computable from a handle's first k (<= 8)
//     reads predicts its byte follow-through with Spearman |rho| >= 0.5, fit on one
//     arm's handles and tested on a different arm's.
//   - NO SEPARATION iff the best feature gives |rho| < 0.5 AND the per-label
//     follow-through distributions overlap at rank-sum AUC < 0.7. That outcome is a
//     documented limitation, reached without another cluster run.
//
// Usage:
//
//	lith-pfreplay [-k 8] [-out handles.csv] hemco=hemco.csv met=met.csv
//
// Two semantics worth knowing before reading any number it prints:
//
//   - Dispatched blocks are CLAMPED AT EOF, exactly as store.Prefetch clamps them.
//     Without that, a deep readahead window against a small object counts blocks
//     that fetched nothing, which drags every score to ~0 and reads as "prefetch
//     never pays off".
//   - Dispatched bytes count DECISIONS, not S3 bytes: several handles read one
//     object, and a handle that re-establishes after a seek asks for blocks it
//     already asked for. The live block store dedupes those; this does not.
//
// Fidelity is reported first and labelled for what it does and does not prove. The
// state-machine check compares Observe's output against a trace column that also
// came from Observe, so it validates the replayed detector but CANNOT catch an error
// in byte accounting — which is the class of bug this tool shipped with first time.
// The denominator sanity check and the optional -issued cross-check are the
// independent ones.
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

const chunkSize = 1 << 20 // lith's cache-chunk granularity, for the straddle feature

type row struct {
	seq         int64
	maxWindow   int64 // the mount's SetMax input for this decision; 0 = pre-#267 trace
	fh          uint64
	size        int64 // object size, for the EOF clamp
	off, length int64
	blk, gap    int64
	path        string // window | parts | footer
	before      string
	after       string
	dispatched  int
}

type traceConfig struct {
	blockSize, maxReadahead, partsMax, smallFile int64
	coverageWindow                               int
	coverageMin, evidenceRatio                   float64
}

// handleScore is one handle's verdict plus the features offered a chance to predict
// it. Fields are exported through the CSV so the maintainer can re-analyse without
// re-running this tool.
type handleScore struct {
	label            string // class (hemco / met) — what the AUC compares
	arm              string // arm within the class — what train/test splits on
	fh               uint64
	objSize          int64
	rows             int
	dispatchedBytes  int64
	usedBytes        int64
	followThrough    float64
	mismatches       int
	meanAbsGapBlocks float64
	maxAbsGapBlocks  float64
	fracLargeGap     float64
	meanReadKiB      float64
	fracMonotonic    float64
	fracStraddle     float64
	// #256 cold-start granularity tax. Counted two ways, because both distinctions
	// were reported as inflating the estimate (and both did):
	//   - waste is NET: chunk bytes the same handle never subsequently read. A
	//     contiguous reader consumes the chunks it cold-fetched, so its real waste is
	//     ~0; charging it chunk-minus-read-length credited a streaming arm with
	//     hundreds of MiB of fiction.
	//   - first-run cold is split from RE-ENTRIES into cold (seq->cold transitions are
	//     real), because only the first run is the pre-decision phase.
	coldFirstRunReads  int
	coldReentryReads   int
	coldNetWasteBytes  int64  // first-run only
	coldGrossWasteByte int64  // chunk - read_len, the inflated figure, kept for comparison
	dispatchDecisions  int    // blocks Observe returned, before the EOF clamp
	firstDiverge       string // the first row where replay and mount disagreed, for diagnosis
	recordedDispatched int    // blocks the MOUNT recorded dispatching, from the trace's own column
	clampedAway        int    // of those, how many fell entirely past the object's end
}

func main() {
	k := flag.Int("k", 8, "number of a handle's first reads the predictive features may use")
	out := flag.String("out", "", "also write per-handle scores and features to this CSV")
	byteExact := flag.Int64("byte-exact-threshold", chunkSize, "largest read eligible for byte-exact fetch, for the cold-start tax estimate")
	minN := flag.Int("min-n", 8, "minimum scored handles per arm for a correlation to count toward the verdict")
	issued := flag.Int64("issued", 0, "the run's lith_prefetch_issued_total, if you have it: the only fully independent check on the replay's denominator")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lith-pfreplay [-k 8] [-out handles.csv] label=trace.csv [label=trace.csv ...]")
		os.Exit(2)
	}

	var all []handleScore
	var totalDispBytes, totalDecisionBlocks, totalRecordedBlocks, blockSize int64
	labels := []string{}
	seenLabel := map[string]bool{}
	for _, arg := range flag.Args() {
		spec, path, ok := strings.Cut(arg, "=")
		if !ok {
			path = arg
			spec = strings.TrimSuffix(filepath.Base(arg), ".csv")
		}
		// class[/arm]: the CLASS is what the AUC compares (hemco vs met) and the ARM is
		// what the train/test split uses. Without the distinction, four traces named
		// hemcoA hemcoB metA metB compared HEMCO against HEMCO.
		label, arm, _ := strings.Cut(spec, "/")
		cfg, rows, err := readTrace(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		scores := scoreTrace(label, arm, cfg, rows, *k, *byteExact)
		fmt.Printf("== class=%s arm=%s (%s)\n", label, arm, path)
		fmt.Printf("   config: block_size=%d max_readahead=%d parts_max=%d small_file=%d coverage=%d/%g evidence_ratio=%g\n",
			cfg.blockSize, cfg.maxReadahead, cfg.partsMax, cfg.smallFile, cfg.coverageWindow, cfg.coverageMin, cfg.evidenceRatio)
		reportFidelity(rows, scores)
		reportDistribution(scores)
		reportColdTax(scores)
		for _, sc := range scores {
			totalDispBytes += sc.dispatchedBytes
			totalRecordedBlocks += int64(sc.recordedDispatched)
			totalDecisionBlocks += int64(sc.dispatchDecisions)
		}
		blockSize = cfg.blockSize
		all = append(all, scores...)
		if !seenLabel[label] {
			seenLabel[label] = true
			labels = append(labels, label)
		}
	}

	reportIssuedCrossCheck(totalDecisionBlocks, totalRecordedBlocks, totalDispBytes, blockSize, *issued)
	reportSeparation(all, labels, *minN)
	if *out != "" {
		if err := writeCSV(*out, all); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
			os.Exit(1)
		}
		fmt.Printf("\nper-handle scores written to %s\n", *out)
	}
}

// readTrace parses the `#` config header and the rows. The header is what makes a
// replay possible at all: without it the tool would be guessing the block size and
// coverage settings that produced the decisions it is trying to reproduce.
func readTrace(path string) (traceConfig, []row, error) {
	f, err := os.Open(path)
	if err != nil {
		return traceConfig{}, nil, err
	}
	defer func() { _ = f.Close() }()

	var cfg traceConfig
	var haveCfg bool
	br := bufio.NewReader(f)
	// Peek past comment lines, collecting config.
	for {
		b, err := br.Peek(1)
		if err != nil || b[0] != '#' {
			break
		}
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			break
		}
		cfg, haveCfg = parseConfig(line)
	}
	if !haveCfg {
		return cfg, nil, fmt.Errorf("no `# lith prefetch trace; block_size=...` header: this trace predates the replayable format (#262) and cannot be replayed faithfully")
	}

	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	recs, err := cr.ReadAll()
	if err != nil {
		return cfg, nil, err
	}
	if len(recs) == 0 {
		return cfg, nil, fmt.Errorf("trace has no column header")
	}
	col := map[string]int{}
	for i, name := range recs[0] {
		col[strings.TrimSpace(name)] = i
	}
	for _, need := range []string{"fh", "off", "len", "blk", "gap", "path", "state_before", "dispatched"} {
		if _, ok := col[need]; !ok {
			return cfg, nil, fmt.Errorf("trace is missing the %q column (need the #262 format)", need)
		}
	}

	var rows []row
	for _, rec := range recs[1:] {
		if len(rec) < len(col) {
			continue
		}
		atoi := func(name string) int64 { v, _ := strconv.ParseInt(rec[col[name]], 10, 64); return v }
		fh, _ := strconv.ParseUint(rec[col["fh"]], 10, 64)
		var size int64
		if _, ok := col["size"]; ok {
			size = atoi("size")
		}
		var seq, maxWin int64
		if _, ok := col["seq"]; ok {
			seq = atoi("seq")
		}
		if _, ok := col["max_window"]; ok {
			maxWin = atoi("max_window")
		}
		rows = append(rows, row{
			seq: seq, maxWindow: maxWin, fh: fh, size: size, off: atoi("off"), length: atoi("len"), blk: atoi("blk"), gap: atoi("gap"),
			path: rec[col["path"]], before: rec[col["state_before"]], after: rec[col["state_after"]], dispatched: int(atoi("dispatched")),
		})
	}
	return cfg, rows, nil
}

func parseConfig(line string) (traceConfig, bool) {
	var c traceConfig
	found := false
	for _, tok := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#")) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		i, _ := strconv.ParseInt(val, 10, 64)
		fv, _ := strconv.ParseFloat(val, 64)
		switch key {
		case "block_size":
			c.blockSize, found = i, true
		case "max_readahead":
			c.maxReadahead = i
		case "parts_max":
			c.partsMax = i
		case "small_file":
			c.smallFile = i
		case "coverage_window":
			c.coverageWindow = int(i)
		case "coverage_min":
			c.coverageMin = fv
		case "evidence_ratio":
			c.evidenceRatio = fv
		}
	}
	return c, found
}

// scoreTrace replays each handle and scores it.
func scoreTrace(label, arm string, cfg traceConfig, rows []row, k int, byteExact int64) []handleScore {
	byHandle := map[uint64][]row{}
	order := []uint64{}
	for _, r := range rows {
		if _, seen := byHandle[r.fh]; !seen {
			order = append(order, r.fh)
		}
		byHandle[r.fh] = append(byHandle[r.fh], r)
	}

	var out []handleScore
	for _, fh := range order {
		hr := byHandle[fh]
		// Replay in DECISION order, not file order. tracePF appends outside the lock
		// that observe() decides under, so rows arrive out of order whenever one handle
		// has concurrent reads — measured at 10.9% of rows with 256 handles, and 0.0%
		// with a single reader, which is why a single-threaded check calls this clean.
		// Feeding the state machine the wrong sequence is exactly what `seq` was added
		// to prevent, and parsing it without sorting left the fix short of the replay.
		if len(hr) > 1 && hr[0].seq > 0 {
			sort.SliceStable(hr, func(i, j int) bool { return hr[i].seq < hr[j].seq })
		}
		s := handleScore{label: label, arm: arm, fh: fh, rows: len(hr)}

		// Replay. Only rows the live mount actually drove the prefetcher with
		// (path=window) are fed to it; the others are real reads that the live code
		// deliberately skipped, and feeding them would replay a different program
		// than the one that ran.
		s.objSize = objSizeOf(hr)
		p := prefetch.New(cfg.maxReadahead)
		p.SetGapMax(cfg.blockSize)
		p.SetCoverage(cfg.coverageWindow, cfg.coverageMin)
		p.SetEvidence(cfg.evidenceRatio, cfg.blockSize)
		// The mount calls pf.open() ONLY for objects larger than partsThreshold
		// (fuse/fs.go: `if fi.Size > f.partsThreshold() && ...`). For a smaller object
		// the prefetcher is never Open()ed, so its first Observe takes the !haveLast
		// path — lastBlock = blockIdx rather than -1, which leaves lastDelta at 0 and
		// makes the strided branch unreachable on read 2. Calling Open() unconditionally
		// replayed a different start and diverged on exactly the handles that never
		// reach sequential/strided: 10 of 6,229 on a real HEMCO trace, all on objects
		// below parts-max.
		partsThreshold := cfg.partsMax
		if partsThreshold <= 0 {
			partsThreshold = cfg.smallFile
		}
		if s.objSize > partsThreshold {
			p.Open()
		}

		type dispatch struct {
			atRow int
			block int64
		}
		var dispatches []dispatch
		for i, r := range hr {
			if r.path != "window" {
				continue
			}
			// The live path calls SetMax(perHandleWindow()) before EVERY Observe, and
			// that input is mount-wide and time-varying. Replaying without it is a
			// different program: measured 2.70x more dispatch than the mount, because
			// the replay assumed the 223-block static cap where the mount used <= 17.
			if r.maxWindow > 0 {
				p.SetMax(r.maxWindow)
			}
			got := p.Observe(r.blk, r.off, r.length, r.gap)
			s.recordedDispatched += r.dispatched
			if len(got) != r.dispatched {
				s.mismatches++
				if s.firstDiverge == "" {
					// Counting mismatches says a replay is wrong; naming the row says why.
					s.firstDiverge = fmt.Sprintf("row %d/%d seq=%d blk=%d off=%d len=%d gap=%d maxwin=%d state %s->%s: mount dispatched %d, replay %d",
						i+1, len(hr), r.seq, r.blk, r.off, r.length, r.gap, r.maxWindow, r.before, r.after, r.dispatched, len(got))
				}
			}
			for _, b := range got {
				dispatches = append(dispatches, dispatch{atRow: i, block: b})
			}
		}

		// The object's size, needed both to decide whether the mount would have called
		// pf.open() and to clamp dispatched blocks at EOF exactly as
		// store.Prefetch does. Without this the denominator counts blocks that fetched
		// NOTHING: at max_readahead=223 against a 14-block object, ~94% of it, which
		// drives every follow-through score to ~0 and reads as "prefetch never pays
		// off". Falls back to the handle's own high-water mark for pre-#267 traces,
		// which under-counts rather than inflating, and is reported as an estimate.
		// Byte follow-through: for each dispatched block, the union of bytes that
		// LATER reads of this same handle covered within it.
		s.dispatchDecisions = len(dispatches)
		for _, d := range dispatches {
			lo := d.block * cfg.blockSize
			if s.objSize > 0 && lo >= s.objSize {
				s.clampedAway++
				continue // past EOF: the live fetch returns without doing anything
			}
			hi := lo + cfg.blockSize
			if s.objSize > 0 && hi > s.objSize {
				hi = s.objSize
			}
			s.dispatchedBytes += hi - lo
			s.usedBytes += coveredBytes(hr[d.atRow+1:], lo, hi)
		}
		if s.dispatchedBytes > 0 {
			s.followThrough = float64(s.usedBytes) / float64(s.dispatchedBytes)
		} else {
			s.followThrough = math.NaN()
		}

		coldTax(&s, hr, byteExact)
		features(&s, hr, cfg.blockSize, k)
		out = append(out, s)
	}
	return out
}

// coveredBytes returns how many bytes of [lo,hi) the given reads cover, counting
// each byte once however many reads touch it.
func coveredBytes(reads []row, lo, hi int64) int64 {
	type iv struct{ a, b int64 }
	var ivs []iv
	for _, r := range reads {
		a, b := r.off, r.off+r.length
		if a < lo {
			a = lo
		}
		if b > hi {
			b = hi
		}
		if a < b {
			ivs = append(ivs, iv{a, b})
		}
	}
	if len(ivs) == 0 {
		return 0
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a < ivs[j].a })
	var total int64
	cur := ivs[0]
	for _, x := range ivs[1:] {
		if x.a > cur.b {
			total += cur.b - cur.a
			cur = x
			continue
		}
		if x.b > cur.b {
			cur.b = x.b
		}
	}
	return total + cur.b - cur.a
}

// objSizeOf returns the object size the trace recorded, or the handle's own
// high-water read offset when the trace predates the size column. The fallback
// under-states the size, which under-states the denominator — the safe direction,
// since the failure being guarded against is an inflated one.
func objSizeOf(hr []row) int64 {
	for _, r := range hr {
		if r.size > 0 {
			return r.size
		}
	}
	var hi int64
	for _, r := range hr {
		if e := r.off + r.length; e > hi {
			hi = e
		}
	}
	return hi
}

// coldTax measures the #256 floor: a read served at whole-chunk granularity purely
// because the handle was not yet classified (a `cold` handle takes `sequential =
// true` in the granularity decision, so a small read pays a whole chunk).
//
// Two corrections over the naive count, both reported as inflating it:
//
//   - Waste is NET — chunk bytes the same handle never reads at all. A contiguous
//     reader consumes what it cold-fetched, so its true waste is ~0; charging it
//     chunk-minus-read-length credits a streaming arm with fiction.
//   - Only the FIRST cold run is the pre-decision phase. A handle can re-enter cold
//     later (seq->cold is a real transition), and those reads are not what the
//     mechanism describes.
//
// Chunks are counted once: a second cold read landing in a chunk already fetched is
// a cache hit and costs nothing.
func coldTax(s *handleScore, hr []row, byteExact int64) {
	firstRun := true
	seen := map[int64]bool{}
	for _, r := range hr {
		if r.before != "cold" {
			firstRun = false // the handle has been classified at least once
			continue
		}
		if r.path != "window" || r.length > byteExact || r.length >= chunkSize {
			continue
		}
		if !firstRun {
			s.coldReentryReads++
			continue
		}
		s.coldFirstRunReads++
		ci := r.off / chunkSize
		if seen[ci] {
			continue // the chunk was already fetched by an earlier cold read
		}
		seen[ci] = true
		lo := ci * chunkSize
		hi := lo + chunkSize
		if s.objSize > 0 && hi > s.objSize {
			hi = s.objSize
		}
		if hi <= lo {
			continue
		}
		s.coldGrossWasteByte += (hi - lo) - r.length
		// Net: everything in the chunk this handle never reads, at any point.
		s.coldNetWasteBytes += (hi - lo) - coveredBytes(hr, lo, hi)
	}
}

// features computes the candidates the agreed rule allows: anything derivable from
// the handle's first k reads.
func features(s *handleScore, hr []row, blockSize int64, k int) {
	if k > len(hr) {
		k = len(hr)
	}
	if k == 0 || blockSize <= 0 {
		return
	}
	first := hr[:k]
	var sumGap, sumLen, mono, straddle, large float64
	for i, r := range first {
		g := math.Abs(float64(r.gap)) / float64(blockSize)
		sumGap += g
		if g > s.maxAbsGapBlocks {
			s.maxAbsGapBlocks = g
		}
		if g > 1 {
			large++
		}
		sumLen += float64(r.length) / 1024
		if i > 0 && r.off > first[i-1].off {
			mono++
		}
		if r.off%chunkSize+r.length > chunkSize {
			straddle++
		}
	}
	n := float64(k)
	s.meanAbsGapBlocks = sumGap / n
	s.fracLargeGap = large / n
	s.meanReadKiB = sumLen / n
	s.fracStraddle = straddle / n
	if k > 1 {
		s.fracMonotonic = mono / float64(k-1)
	}
}

func reportFidelity(rows []row, scores []handleScore) {
	var mism, handles int
	var dispBytes, objBytes int64
	seenKey := map[int64]bool{}
	for _, sc := range scores {
		if sc.mismatches > 0 {
			mism++
		}
		handles++
		dispBytes += sc.dispatchedBytes
		if !seenKey[sc.objSize] {
			seenKey[sc.objSize] = true
			objBytes += sc.objSize
		}
	}
	pathCount := map[string]int{}
	haveSize := false
	for _, r := range rows {
		pathCount[r.path]++
		if r.size > 0 {
			haveSize = true
		}
	}
	fmt.Printf("   rows: %d (window=%d parts=%d footer=%d)  handles: %d\n",
		len(rows), pathCount["window"], pathCount["parts"], pathCount["footer"], handles)
	haveMaxWin := false
	for _, r := range rows {
		if r.maxWindow > 0 {
			haveMaxWin = true
			break
		}
	}
	if !haveMaxWin {
		fmt.Printf("   ** no `max_window` column: the replay runs the STATIC cap where the mount applied a\n")
		fmt.Printf("      time-varying per-handle budget share before every Observe. Expect inflated dispatch and\n")
		fmt.Printf("      fidelity mismatches that are this absence, not lith's behaviour. Recapture to score. **\n")
	}
	if !haveSize {
		fmt.Printf("   ** no `size` column: object sizes ESTIMATED from each handle's highest read offset.\n")
		fmt.Printf("      The EOF clamp is therefore approximate (it under-states, not over-states). Recapture with a #267 binary.\n")
	}

	// State-machine fidelity. Deliberately labelled for what it does and does not
	// prove: both sides of this comparison originate in Observe, so it CANNOT catch
	// an error in how dispatched blocks are converted to bytes. That is exactly the
	// class of bug an earlier version of this tool shipped with, so the check below
	// it is the one that matters.
	if mism == 0 {
		fmt.Printf("   state-machine fidelity: OK — replayed dispatch DECISIONS match the recorded ones on every handle\n")
	} else {
		fmt.Printf("   state-machine fidelity: ** MISMATCH on %d/%d handles ** — the detector replay is wrong; everything below is void.\n", mism, handles)
		shown := 0
		for _, sc := range scores {
			if sc.firstDiverge == "" || shown >= 3 {
				continue
			}
			fmt.Printf("     fh=%d first divergence: %s\n", sc.fh, sc.firstDiverge)
			shown++
		}
	}
	fmt.Printf("     (this compares Observe against a column produced by Observe: it validates the state machine, NOT the byte accounting)\n")

	// Independent sanity check on the denominator — but only when object sizes are
	// real. Without the `size` column each handle estimates its own object, so the
	// same object is counted once per handle: "distinct objects" INFLATES while
	// decisions collapse, and the ratio moves the wrong way (measured: 2.02x becomes
	// 0.07x, which reads as "plenty of headroom" at the moment it is broken). A
	// number that points the wrong way is worse than no number, so it is withheld.
	if !haveSize {
		fmt.Printf("   denominator sanity: SUPPRESSED — without `size` this ratio inverts and reads reassuringly while broken.\n")
	} else if objBytes > 0 {
		ratio := float64(dispBytes) / float64(objBytes)
		// >1x is normal and not a bug: several handles read one object, and a handle
		// that re-establishes after a seek re-dispatches blocks it already asked for.
		// The live block store dedupes those; this counts decisions.
		fmt.Printf("   denominator sanity: %.1f MiB of dispatch DECISIONS against %.1f MiB of distinct objects (%.2fx; >1x is normal)\n",
			float64(dispBytes)/(1<<20), float64(objBytes)/(1<<20), ratio)
		if ratio > 4 {
			fmt.Printf("     ** IMPLAUSIBLE: a mount cannot usefully dispatch many times an object's size. Suspect the EOF clamp. **\n")
		}
	}

	// Population bias: a handle whose every dispatch clamps away leaves the
	// follow-through population entirely. With estimated sizes that happens to the
	// handles that stopped early — which are the most wasteful ones, i.e. exactly the
	// population the question is about. Report it rather than let n shrink silently.
	var dropped, decided int
	for _, sc := range scores {
		if sc.dispatchDecisions > 0 {
			decided++
			if sc.dispatchedBytes == 0 {
				dropped++
			}
		}
	}
	if dropped > 0 {
		fmt.Printf("   ** %d of %d handles that dispatched prefetch scored NOTHING: every dispatch clamped past EOF.\n", dropped, decided)
		if !haveSize {
			fmt.Printf("      With estimated sizes this drops the handles that stopped early — the most wasteful ones. Recapture with `size`. **\n")
		} else {
			fmt.Printf("      With real sizes this means the detector dispatched only past EOF. **\n")
		}
	}

}

// reportIssuedCrossCheck compares the TOTAL replayed chunk-decisions against the
// run's single live counter. Doing this per trace was wrong: with N traces from one
// mount, each arm's own count was printed beside the whole run's total, so the line
// that exists to catch a 12x error was itself off by up to Nx.
// reportIssuedCrossCheck decomposes the replay-vs-mount gap into its two factors,
// because a single ratio blames the wrong thing. A 9.25x gap on a real capture was
// read as ~13x sharing when it was 2.70x replay-window inflation (a missing per-call
// input) times 3.43x genuine dedup. The mount's own `dispatched` column is the pivot
// that separates them, and it needs no new column.
func reportIssuedCrossCheck(totalDecisionBlocks, totalRecordedBlocks, totalDispBytes, blockSize, issued int64) {
	perBlock := blockSize / chunkSize
	if perBlock <= 0 {
		return
	}
	decided := totalDecisionBlocks * perBlock // replay decisions, pre-EOF-clamp
	mount := totalRecordedBlocks * perBlock   // what the mount's own column recorded
	fetchable := totalDispBytes / chunkSize   // replay decisions that could fetch anything

	if mount <= 0 {
		return
	}
	fmt.Printf("\n   decomposition of the replay-vs-mount gap:\n")
	fmt.Printf("     replay decisions   %8d chunks (pre-EOF-clamp)   -> %.2fx the mount's, %.2fx of which is past EOF\n",
		decided, float64(decided)/float64(mount), float64(decided)/math.Max(float64(fetchable), 1))
	fmt.Printf("     replay fetchable   %8d chunks                   -> %.2fx  <- window/input inflation\n",
		fetchable, float64(fetchable)/float64(mount))
	fmt.Printf("     mount decided      %8d chunks (its own column)\n", mount)
	if issued > 0 {
		fmt.Printf("     mount fetched      %8d chunks (lith_prefetch_issued_total) -> %.2fx  <- genuine sharing dedup\n",
			issued, float64(mount)/float64(issued))
		fmt.Printf("     Per-handle follow-through understates prefetch's value by about that dedup factor.\n")
	}
	if fetchable > mount*3/2 {
		fmt.Printf("     ** the replay would fetch far more than the mount decided, so a per-call INPUT differs.\n")
		fmt.Printf("        The known one is perHandleWindow (SetMax before every live Observe); a trace without a\n")
		fmt.Printf("        max_window column makes the replay run the static cap. Fidelity, not dedup. **\n")
	}
}

func reportDistribution(scores []handleScore) {
	var ft []float64
	for _, s := range scores {
		if !math.IsNaN(s.followThrough) {
			ft = append(ft, s.followThrough)
		}
	}
	if len(ft) == 0 {
		fmt.Printf("   byte follow-through: no handle dispatched any prefetch\n")
		return
	}
	cp := append([]float64{}, ft...)
	fmt.Printf("   byte follow-through over %d prefetching handles: median %.3f  mean %.3f  p10 %.3f  p90 %.3f\n",
		len(ft), quantile(cp, 0.5), mean(ft), quantile(append([]float64{}, ft...), 0.10), quantile(append([]float64{}, ft...), 0.90))
}

func reportColdTax(scores []handleScore) {
	var first, reentry int
	var net, gross int64
	for _, s := range scores {
		first += s.coldFirstRunReads
		reentry += s.coldReentryReads
		net += s.coldNetWasteBytes
		gross += s.coldGrossWasteByte
	}
	fmt.Printf("   cold-start granularity tax (#256 floor): %d first-run pre-decision small reads (+%d cold re-entries, excluded)\n",
		first, reentry)
	fmt.Printf("     NET waste %.1f MiB (chunk bytes never read by that handle)   [gross, uncorrected: %.1f MiB]\n",
		float64(net)/(1<<20), float64(gross)/(1<<20))
}

// reportSeparation applies the pre-registered rule.
func reportSeparation(all []handleScore, labels []string, minN int) {
	fmt.Printf("\n== pre-registered verdict (#256)\n")

	// THE VERDICT IS COMPUTED ON FAITHFUL HANDLES ONLY.
	//
	// An earlier build printed "everything below is void" from the fidelity gate and
	// then, eleven lines later, "VERDICT: SEPARATION" — disagreeing with itself on one
	// page, on real data. --min-n did not help because it counts SCORED handles, and a
	// handle whose replay diverged is scored; it is just scored wrong. Nothing
	// connected the verdict to the gate. It does now: a handle whose replayed dispatch
	// decisions differ from the mount's recorded ones is replaying a different program,
	// so its follow-through is not evidence about lith's behaviour and cannot vote.
	var faithful []handleScore
	unfaithful := 0
	for _, sc := range all {
		if sc.mismatches == 0 {
			faithful = append(faithful, sc)
			continue
		}
		if !math.IsNaN(sc.followThrough) {
			unfaithful++
		}
	}
	if unfaithful > 0 {
		fmt.Printf("   fidelity filter: %d scored handles excluded (replay diverged from the mount);\n", unfaithful)
		fmt.Printf("     the verdict below is computed on the faithful remainder ONLY. A diverging handle is\n")
		fmt.Printf("     replaying a different program, so its score is not evidence about lith.\n")
	}
	all = faithful
	byLabel := map[string][]float64{}
	for _, s := range all {
		if !math.IsNaN(s.followThrough) {
			byLabel[s.label] = append(byLabel[s.label], s.followThrough)
		}
	}

	auc := math.NaN()
	for _, lab := range labels {
		if len(byLabel[lab]) == 0 {
			fmt.Printf("   ** class %q has NO handles that dispatched prefetch: the AUC half of the rule cannot be\n", lab)
			fmt.Printf("      evaluated for it, and \"no separation\" must NOT be concluded from its absence. **\n")
		}
	}
	switch {
	case len(labels) < 2:
		fmt.Printf("   AUC: needs two CLASSES (e.g. hemco/a=1.csv hemco/b=2.csv met/a=3.csv met/b=4.csv)\n")
	case len(labels) > 2:
		fmt.Printf("   AUC: %d classes given; the rule is defined for two. Classes: %v\n", len(labels), labels)
	default:
		a, b := byLabel[labels[0]], byLabel[labels[1]]
		if v, ok := aucMannWhitney(a, b); ok {
			auc = math.Max(v, 1-v)
			fmt.Printf("   AUC(%s vs %s) on byte follow-through = %.3f  (n=%d vs %d)\n", labels[0], labels[1], auc, len(a), len(b))
			fmt.Printf("     (note: the 0.7 bar is undemanding — a uniform shift of one unit clears it at 0.719)\n")
		}
	}

	type feat struct {
		name string
		get  func(handleScore) float64
	}
	feats := []feat{
		{"mean_abs_gap_blocks", func(s handleScore) float64 { return s.meanAbsGapBlocks }},
		{"max_abs_gap_blocks", func(s handleScore) float64 { return s.maxAbsGapBlocks }},
		{"frac_large_gap", func(s handleScore) float64 { return s.fracLargeGap }},
		{"mean_read_kib", func(s handleScore) float64 { return s.meanReadKiB }},
		{"frac_monotonic", func(s handleScore) float64 { return s.fracMonotonic }},
		{"frac_straddle", func(s handleScore) float64 { return s.fracStraddle }},
	}

	best := 0.0
	bestName := "(none)"
	fmt.Printf("   Spearman rho per class/arm (train on one arm, test on another — the agreed rule).\n")
	fmt.Printf("     A value only counts toward the verdict when its arm has >= %d scored handles AND >= %d\n", minN, minDistinct)
	fmt.Printf("     distinct values on BOTH axes: n=3 with a two-way tie is monotone by construction, not a finding.\n")
	for _, ft := range feats {
		line := "     " + fmt.Sprintf("%-22s", ft.name)
		qualified := 0
		for _, lab := range labels {
			arms := map[string][]handleScore{}
			armOrder := []string{}
			for _, sc := range all {
				if sc.label != lab || math.IsNaN(sc.followThrough) {
					continue
				}
				if _, ok := arms[sc.arm]; !ok {
					armOrder = append(armOrder, sc.arm)
				}
				arms[sc.arm] = append(arms[sc.arm], sc)
			}
			if len(armOrder) == 0 {
				line += fmt.Sprintf("  %s=(none)", lab)
				continue
			}
			for _, a := range armOrder {
				var xs, ys []float64
				for _, sc := range arms[a] {
					xs = append(xs, ft.get(sc))
					ys = append(ys, sc.followThrough)
				}
				name := lab
				if a != "" {
					name = lab + "/" + a
				}
				r, ok := spearman(xs, ys)
				if !ok {
					line += fmt.Sprintf("  %s=n/a", name)
					continue
				}
				why := degenerate(xs, ys, minN)
				if why != "" {
					// Printed, but explicitly not counted: a degenerate fit that happens to
					// be monotone must not become a SEPARATION verdict.
					line += fmt.Sprintf("  %s=%+.3f[%s]", name, r, why)
					continue
				}
				line += fmt.Sprintf("  %s=%+.3f", name, r)
				if math.Abs(r) >= 0.5 {
					qualified++
				}
				if math.Abs(r) > best {
					best, bestName = math.Abs(r), ft.name
				}
			}
		}
		if qualified >= 2 {
			line += "   <- |rho|>=0.5 on 2+ QUALIFYING arms"
		}
		fmt.Println(line)
	}

	fmt.Printf("\n   best |rho| = %.3f (%s); threshold 0.5\n", best, bestName)

	// Distinguish "measured no relationship" from "could not measure one". After the
	// fidelity filter a class can be left with a handful of handles, and NO SEPARATION
	// asserted from n=3 is not the pre-registered finding — it is an absence of data
	// wearing the finding's clothes.
	thin := []string{}
	for _, lab := range labels {
		// n == 0 counts as thin too: a class with no faithful scored handles cannot
		// support either branch, and "no separation" must never be concluded from an
		// absence of data.
		if n := len(byLabel[lab]); n < minN {
			thin = append(thin, fmt.Sprintf("%s(n=%d)", lab, n))
		}
	}
	if len(thin) > 0 {
		fmt.Printf("   VERDICT: UNEVALUABLE — after the fidelity filter these classes are below --min-n=%d: %s.\n",
			minN, strings.Join(thin, ", "))
		fmt.Printf("            Neither branch of the rule may be claimed: |rho| < 0.5 from n=3 is an absence of\n")
		fmt.Printf("            data, not a measured absence of relationship. Fix fidelity, then re-score.\n")
		return
	}
	switch {
	case best >= 0.5:
		fmt.Printf("   VERDICT: SEPARATION — a feature of a handle's first reads predicts byte follow-through.\n")
		fmt.Printf("            Validate it out-of-sample (fit one arm, test another) before designing a policy on it.\n")
	case !math.IsNaN(auc) && auc >= 0.7:
		fmt.Printf("   VERDICT: PARTIAL — no single early feature predicts follow-through (|rho| < 0.5), but the\n")
		fmt.Printf("            per-label distributions do separate (AUC %.3f >= 0.7): the difference is real and\n", auc)
		fmt.Printf("            observable somewhere, just not in these features.\n")
	case !math.IsNaN(auc):
		fmt.Printf("   VERDICT: NO SEPARATION — |rho| < 0.5 and AUC %.3f < 0.7.\n", auc)
		fmt.Printf("            Nothing in recorded reads distinguishes the handles whose prefetch pays off.\n")
		fmt.Printf("            This is the documented-limitation outcome, reached without another cluster run.\n")
	default:
		fmt.Printf("   VERDICT: inconclusive — supply two labelled traces for the AUC half of the rule.\n")
	}
}

func writeCSV(path string, all []handleScore) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"label", "arm", "fh", "rows", "dispatched_bytes", "used_bytes", "byte_follow_through", "replay_mismatches",
		"mean_abs_gap_blocks", "max_abs_gap_blocks", "frac_large_gap", "mean_read_kib", "frac_monotonic",
		"frac_straddle", "obj_size", "cold_first_run_reads", "cold_reentry_reads",
		"cold_net_waste_bytes", "cold_gross_waste_bytes",
	}); err != nil {
		return err
	}
	for _, s := range all {
		if err := w.Write([]string{
			s.label, s.arm, strconv.FormatUint(s.fh, 10), strconv.Itoa(s.rows),
			strconv.FormatInt(s.dispatchedBytes, 10), strconv.FormatInt(s.usedBytes, 10),
			strconv.FormatFloat(s.followThrough, 'f', 6, 64), strconv.Itoa(s.mismatches),
			strconv.FormatFloat(s.meanAbsGapBlocks, 'f', 6, 64), strconv.FormatFloat(s.maxAbsGapBlocks, 'f', 6, 64),
			strconv.FormatFloat(s.fracLargeGap, 'f', 6, 64), strconv.FormatFloat(s.meanReadKiB, 'f', 3, 64),
			strconv.FormatFloat(s.fracMonotonic, 'f', 6, 64), strconv.FormatFloat(s.fracStraddle, 'f', 6, 64),
			strconv.FormatInt(s.objSize, 10), strconv.Itoa(s.coldFirstRunReads), strconv.Itoa(s.coldReentryReads),
			strconv.FormatInt(s.coldNetWasteBytes, 10), strconv.FormatInt(s.coldGrossWasteByte, 10),
		}); err != nil {
			return err
		}
	}
	return nil
}
