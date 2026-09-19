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
// Replay fidelity is reported first and deliberately loudly: if the replayed
// dispatch counts disagree with what the live mount recorded, every number after it
// is void.
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
	fh          uint64
	off, length int64
	blk, gap    int64
	path        string // window | parts | footer
	before      string
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
	label                   string
	fh                      uint64
	rows                    int
	dispatchedBytes         int64
	usedBytes               int64
	followThrough           float64
	mismatches              int
	meanAbsGapBlocks        float64
	maxAbsGapBlocks         float64
	fracLargeGap            float64
	meanReadKiB             float64
	fracMonotonic           float64
	fracStraddle            float64
	coldSmallReads          int // the #256 cold-start granularity tax, countable here
	coldSmallReadWasteBytes int64
}

func main() {
	k := flag.Int("k", 8, "number of a handle's first reads the predictive features may use")
	out := flag.String("out", "", "also write per-handle scores and features to this CSV")
	byteExact := flag.Int64("byte-exact-threshold", chunkSize, "largest read eligible for byte-exact fetch, for the cold-start tax estimate")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lith-pfreplay [-k 8] [-out handles.csv] label=trace.csv [label=trace.csv ...]")
		os.Exit(2)
	}

	var all []handleScore
	labels := []string{}
	for _, arg := range flag.Args() {
		label, path, ok := strings.Cut(arg, "=")
		if !ok {
			// No explicit label: name the arm after the file, so a single-trace run
			// still reports something readable.
			path = arg
			label = strings.TrimSuffix(filepath.Base(arg), ".csv")
		}
		cfg, rows, err := readTrace(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		scores := scoreTrace(label, cfg, rows, *k, *byteExact)
		fmt.Printf("== %s (%s)\n", label, path)
		fmt.Printf("   config: block_size=%d max_readahead=%d parts_max=%d small_file=%d coverage=%d/%g evidence_ratio=%g\n",
			cfg.blockSize, cfg.maxReadahead, cfg.partsMax, cfg.smallFile, cfg.coverageWindow, cfg.coverageMin, cfg.evidenceRatio)
		reportFidelity(rows, scores)
		reportDistribution(scores)
		reportColdTax(scores)
		all = append(all, scores...)
		labels = append(labels, label)
	}

	reportSeparation(all, labels)
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
		rows = append(rows, row{
			fh: fh, off: atoi("off"), length: atoi("len"), blk: atoi("blk"), gap: atoi("gap"),
			path: rec[col["path"]], before: rec[col["state_before"]], dispatched: int(atoi("dispatched")),
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
func scoreTrace(label string, cfg traceConfig, rows []row, k int, byteExact int64) []handleScore {
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
		s := handleScore{label: label, fh: fh, rows: len(hr)}

		// Replay. Only rows the live mount actually drove the prefetcher with
		// (path=window) are fed to it; the others are real reads that the live code
		// deliberately skipped, and feeding them would replay a different program
		// than the one that ran.
		p := prefetch.New(cfg.maxReadahead)
		p.SetGapMax(cfg.blockSize)
		p.SetCoverage(cfg.coverageWindow, cfg.coverageMin)
		p.SetEvidence(cfg.evidenceRatio, cfg.blockSize)
		p.Open()

		type dispatch struct {
			atRow int
			block int64
		}
		var dispatches []dispatch
		for i, r := range hr {
			if r.path != "window" {
				continue
			}
			got := p.Observe(r.blk, r.off, r.length, r.gap)
			if len(got) != r.dispatched {
				s.mismatches++
			}
			for _, b := range got {
				dispatches = append(dispatches, dispatch{atRow: i, block: b})
			}
		}

		// Byte follow-through: for each dispatched block, the union of bytes that
		// LATER reads of this same handle covered within it.
		for _, d := range dispatches {
			lo := d.block * cfg.blockSize
			hi := lo + cfg.blockSize
			s.dispatchedBytes += cfg.blockSize
			s.usedBytes += coveredBytes(hr[d.atRow+1:], lo, hi)
		}
		if s.dispatchedBytes > 0 {
			s.followThrough = float64(s.usedBytes) / float64(s.dispatchedBytes)
		} else {
			s.followThrough = math.NaN()
		}

		s.coldSmallReads, s.coldSmallReadWasteBytes = coldTax(hr, byteExact)
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

// coldTax counts reads served at whole-chunk granularity purely because the handle
// was not yet classified — the #256 floor mechanism. A `cold` handle gets
// `sequential = true` in the granularity decision, so a small read pays a whole
// chunk. Waste is the chunk minus what the read asked for.
func coldTax(hr []row, byteExact int64) (n int, waste int64) {
	for _, r := range hr {
		if r.path == "window" && r.before == "cold" && r.length <= byteExact && r.length < chunkSize {
			n++
			waste += chunkSize - r.length
		}
	}
	return n, waste
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
	for _, s := range scores {
		if s.mismatches > 0 {
			mism++
		}
		handles++
	}
	pathCount := map[string]int{}
	for _, r := range rows {
		pathCount[r.path]++
	}
	fmt.Printf("   rows: %d (window=%d parts=%d footer=%d)  handles: %d\n",
		len(rows), pathCount["window"], pathCount["parts"], pathCount["footer"], handles)
	if mism == 0 {
		fmt.Printf("   replay fidelity: OK — replayed dispatch counts match the live mount on every handle\n")
		return
	}
	fmt.Printf("   replay fidelity: ** MISMATCH on %d/%d handles ** — every number below is void until this is zero.\n", mism, handles)
	fmt.Printf("     (a replay that does not reproduce the live decisions cannot score a hypothetical policy)\n")
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
	var n int
	var waste int64
	for _, s := range scores {
		n += s.coldSmallReads
		waste += s.coldSmallReadWasteBytes
	}
	fmt.Printf("   cold-start granularity tax (#256 floor): %d pre-decision small reads, %.1f MiB wasted\n",
		n, float64(waste)/(1<<20))
}

// reportSeparation applies the pre-registered rule.
func reportSeparation(all []handleScore, labels []string) {
	fmt.Printf("\n== pre-registered verdict (#256)\n")
	byLabel := map[string][]float64{}
	for _, s := range all {
		if !math.IsNaN(s.followThrough) {
			byLabel[s.label] = append(byLabel[s.label], s.followThrough)
		}
	}

	auc := math.NaN()
	if len(labels) >= 2 {
		a, b := byLabel[labels[0]], byLabel[labels[1]]
		if v, ok := aucMannWhitney(a, b); ok {
			auc = math.Max(v, 1-v)
			fmt.Printf("   AUC(%s vs %s) on byte follow-through = %.3f  (n=%d vs %d)\n", labels[0], labels[1], auc, len(a), len(b))
		}
	} else {
		fmt.Printf("   AUC: needs two labelled traces (e.g. hemco=a.csv met=b.csv)\n")
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
	fmt.Printf("   Spearman rho of each first-%s-read feature against byte follow-through:\n", "k")
	for _, ft := range feats {
		line := "     " + fmt.Sprintf("%-22s", ft.name)
		holds := 0
		for _, lab := range labels {
			var xs, ys []float64
			for _, s := range all {
				if s.label == lab && !math.IsNaN(s.followThrough) {
					xs = append(xs, ft.get(s))
					ys = append(ys, s.followThrough)
				}
			}
			if r, ok := spearman(xs, ys); ok {
				line += fmt.Sprintf("  %s=%+.3f", lab, r)
				if math.Abs(r) >= 0.5 {
					holds++
				}
				if math.Abs(r) > best {
					best, bestName = math.Abs(r), ft.name
				}
			} else {
				line += fmt.Sprintf("  %s=n/a", lab)
			}
		}
		if holds >= 2 {
			line += "   <- holds on both arms"
		}
		fmt.Println(line)
	}

	fmt.Printf("\n   best |rho| = %.3f (%s); threshold 0.5\n", best, bestName)
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
		"label", "fh", "rows", "dispatched_bytes", "used_bytes", "byte_follow_through", "replay_mismatches",
		"mean_abs_gap_blocks", "max_abs_gap_blocks", "frac_large_gap", "mean_read_kib", "frac_monotonic",
		"frac_straddle", "cold_small_reads", "cold_small_read_waste_bytes",
	}); err != nil {
		return err
	}
	for _, s := range all {
		if err := w.Write([]string{
			s.label, strconv.FormatUint(s.fh, 10), strconv.Itoa(s.rows),
			strconv.FormatInt(s.dispatchedBytes, 10), strconv.FormatInt(s.usedBytes, 10),
			strconv.FormatFloat(s.followThrough, 'f', 6, 64), strconv.Itoa(s.mismatches),
			strconv.FormatFloat(s.meanAbsGapBlocks, 'f', 6, 64), strconv.FormatFloat(s.maxAbsGapBlocks, 'f', 6, 64),
			strconv.FormatFloat(s.fracLargeGap, 'f', 6, 64), strconv.FormatFloat(s.meanReadKiB, 'f', 3, 64),
			strconv.FormatFloat(s.fracMonotonic, 'f', 6, 64), strconv.FormatFloat(s.fracStraddle, 'f', 6, 64),
			strconv.Itoa(s.coldSmallReads), strconv.FormatInt(s.coldSmallReadWasteBytes, 10),
		}); err != nil {
			return err
		}
	}
	return nil
}
