// SPDX-License-Identifier: Apache-2.0

package bgzf

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"reflect"
	"testing"
)

// --- synthetic index builders (I control the byte layout; no samtools dep) ---

type refSpec struct {
	bin    uint32
	chunks [][2]uint64 // (beg,end) virtual offsets
	linear []uint64
}

func vo(coff, uoff uint64) uint64 { return coff<<16 | uoff }

func buildBAI(refs []refSpec) []byte {
	var b bytes.Buffer
	b.WriteString("BAI\x01")
	le := binary.LittleEndian
	w32 := func(v uint32) { var x [4]byte; le.PutUint32(x[:], v); b.Write(x[:]) }
	w64 := func(v uint64) { var x [8]byte; le.PutUint64(x[:], v); b.Write(x[:]) }
	w32(uint32(len(refs)))
	for _, r := range refs {
		w32(1) // n_bin
		w32(r.bin)
		w32(uint32(len(r.chunks)))
		for _, c := range r.chunks {
			w64(c[0])
			w64(c[1])
		}
		w32(uint32(len(r.linear))) // n_intv
		for _, l := range r.linear {
			w64(l)
		}
	}
	return b.Bytes()
}

func buildTBI(refs []refSpec) []byte {
	var b bytes.Buffer
	b.WriteString("TBI\x01")
	le := binary.LittleEndian
	w32 := func(v uint32) { var x [4]byte; le.PutUint32(x[:], v); b.Write(x[:]) }
	w64 := func(v uint64) { var x [8]byte; le.PutUint64(x[:], v); b.Write(x[:]) }
	for i := 0; i < 6; i++ {
		w32(0) // format, col_seq, col_beg, col_end, meta, skip
	}
	w32(0) // l_nm
	w32(uint32(len(refs)))
	for _, r := range refs {
		w32(1)
		w32(r.bin)
		w32(uint32(len(r.chunks)))
		for _, c := range r.chunks {
			w64(c[0])
			w64(c[1])
		}
		w32(uint32(len(r.linear)))
		for _, l := range r.linear {
			w64(l)
		}
	}
	return gzipBytes(b.Bytes())
}

func gzipBytes(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return out.Bytes()
}

// --- tests ---

func TestIndexCandidates(t *testing.T) {
	cases := map[string]IndexKind{
		"a/x.cram":     KindCRAI,
		"a/x.bam":      KindBAI,
		"rel/y.vcf.gz": KindTBI,
		"z.bcf":        KindCSI,
		"nope.txt":     KindNone,
	}
	for key, wantFirst := range cases {
		cs := IndexCandidates(key)
		if wantFirst == KindNone {
			if cs != nil {
				t.Errorf("%s: expected no candidates, got %v", key, cs)
			}
			continue
		}
		if len(cs) == 0 || cs[0].Kind != wantFirst {
			t.Errorf("%s: first candidate kind = %v, want %v (cands=%v)", key, cs, wantFirst, cs)
		}
	}
	// Both suffix conventions are offered.
	cs := IndexCandidates("d/x.bam")
	if len(cs) < 2 || cs[0].Key != "d/x.bam.bai" || cs[1].Key != "d/x.bai" {
		t.Fatalf("bam candidates = %v, want x.bam.bai then x.bai", cs)
	}
}

func TestBAIRegionRanges(t *testing.T) {
	// Region [0,16384) → level-5 bin 4681. Put a chunk there; linear floor 0.
	bai := buildBAI([]refSpec{{
		bin:    4681,
		chunks: [][2]uint64{{vo(1000, 0), vo(2000, 100)}},
		linear: []uint64{0},
	}})
	ix, ok := ParseBAI(bai)
	if !ok || ix.NRef() != 1 {
		t.Fatalf("ParseBAI ok=%v nref=%d", ok, ix.NRef())
	}
	got := ix.RegionRanges(0, 0, 16384, 1<<30)
	want := []Range{{Start: 1000, End: 2000 + maxBlockSize}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RegionRanges = %v, want %v", got, want)
	}
	// A region on a different, empty part yields nothing; out-of-range ref too.
	if r := ix.RegionRanges(5, 0, 16384, 1<<30); r != nil {
		t.Fatalf("out-of-range ref = %v, want nil", r)
	}
	// Linear floor excludes a chunk that ends below it.
	bai2 := buildBAI([]refSpec{{bin: 4681, chunks: [][2]uint64{{vo(10, 0), vo(20, 0)}}, linear: []uint64{vo(100, 0)}}})
	ix2, _ := ParseBAI(bai2)
	if r := ix2.RegionRanges(0, 0, 16384, 1<<30); r != nil {
		t.Fatalf("linear-floor-excluded chunk still returned: %v", r)
	}
}

func TestBAICoalesce(t *testing.T) {
	// Two adjacent chunks in overlapping bins coalesce into one range.
	bai := buildBAI([]refSpec{{
		bin:    4681,
		chunks: [][2]uint64{{vo(1000, 0), vo(1500, 0)}, {vo(1500, 0), vo(2000, 0)}},
		linear: []uint64{0},
	}})
	ix, _ := ParseBAI(bai)
	got := ix.RegionRanges(0, 0, 16384, 1<<30)
	if len(got) != 1 || got[0].Start != 1000 {
		t.Fatalf("coalesced = %v, want one range from 1000", got)
	}
}

func TestParseTBI(t *testing.T) {
	tbi := buildTBI([]refSpec{{bin: 4681, chunks: [][2]uint64{{vo(500, 0), vo(600, 0)}}, linear: []uint64{0}}})
	ix, ok := ParseTBI(tbi)
	if !ok {
		t.Fatal("ParseTBI failed on a valid image")
	}
	got := ix.RegionRanges(0, 0, 16384, 1<<30)
	if len(got) != 1 || got[0].Start != 500 {
		t.Fatalf("TBI RegionRanges = %v, want one range from 500", got)
	}
}

func TestParseCRAI(t *testing.T) {
	raw := gzipBytes([]byte(
		"0\t0\t1000\t100\t50\t2000\n" + // ref0, [0,1000), container@100
			"0\t5000\t1000\t3000\t50\t2000\n" + // ref0, [5000,6000)
			"1\t0\t1000\t9000\t50\t2000\n")) // ref1
	ix, ok := ParseCRAI(raw)
	if !ok || ix.Len() != 3 {
		t.Fatalf("ParseCRAI ok=%v len=%d", ok, ix.Len())
	}
	// Region ref0 [0,500) overlaps only the first slice; slice-precise start is
	// containerOffset(100)+sliceOffset(50)=150, size 2000 → [150,2150).
	got := ix.RegionRanges(0, 0, 500, 1<<30)
	if len(got) != 1 || got[0].Start != 150 || got[0].End != 2150 {
		t.Fatalf("CRAI RegionRanges = %v, want one slice-precise range [150,2150)", got)
	}
	// A region on ref1 uses ref1's slice only (container@9000 + 50).
	got = ix.RegionRanges(1, 0, 500, 1<<30)
	if len(got) != 1 || got[0].Start != 9050 {
		t.Fatalf("CRAI ref1 = %v, want from 9050", got)
	}
}

func TestParsersRejectMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]byte) bool
		in   []byte
	}{
		{"bai-empty", func(b []byte) bool { _, ok := ParseBAI(b); return ok }, nil},
		{"bai-badmagic", func(b []byte) bool { _, ok := ParseBAI(b); return ok }, []byte("XXXX\x00\x00\x00\x00")},
		{"bai-truncated", func(b []byte) bool { _, ok := ParseBAI(b); return ok }, []byte("BAI\x01\xff\xff")},
		{"tbi-notgzip", func(b []byte) bool { _, ok := ParseTBI(b); return ok }, []byte("not gzip")},
		{"csi-notgzip", func(b []byte) bool { _, ok := ParseCSI(b); return ok }, []byte("not gzip")},
		{"crai-notgzip", func(b []byte) bool { _, ok := ParseCRAI(b); return ok }, []byte("not gzip")},
	} {
		if tc.fn(tc.in) {
			t.Errorf("%s: parser accepted malformed input", tc.name)
		}
	}
}

// FuzzParseTBI / FuzzParseCRAI assert the parsers never panic on arbitrary bytes
// and that any returned index is internally consistent (#101).
func FuzzParseTBI(f *testing.F) {
	f.Add(buildTBI([]refSpec{{bin: 4681, chunks: [][2]uint64{{vo(1, 0), vo(2, 0)}}, linear: []uint64{0}}}))
	f.Add([]byte("TBI\x01"))
	f.Add(gzipBytes([]byte("TBI\x01garbage")))
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, ok := ParseTBI(data)
		if ok {
			_ = ix.RegionRanges(0, 0, 1<<20, 1<<30)
			_ = ix.RegionRanges(-1, 0, 10, 100)
		}
	})
}

func FuzzParseCRAI(f *testing.F) {
	f.Add(gzipBytes([]byte("0\t0\t100\t10\t5\t20\n")))
	f.Add(gzipBytes([]byte("garbage\tlines\there\n1\t2\n")))
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, ok := ParseCRAI(data)
		if ok {
			_ = ix.RegionRanges(0, 0, 1<<20, 1<<30)
		}
	})
}

func FuzzParseBAI(f *testing.F) {
	f.Add(buildBAI([]refSpec{{bin: 4681, chunks: [][2]uint64{{vo(1, 0), vo(2, 0)}}, linear: []uint64{0}}}))
	f.Add([]byte("BAI\x01\x01\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, ok := ParseBAI(data)
		if ok {
			_ = ix.RegionRanges(0, 0, 1<<20, 1<<30)
		}
	})
}
