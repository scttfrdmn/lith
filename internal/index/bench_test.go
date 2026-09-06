// SPDX-License-Identifier: Apache-2.0

package index

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// benchKeys returns the synthetic key count for build benchmarks, overridable
// with LITH_BENCH_KEYS (default 1,000,000).
func benchKeys() int {
	if v := os.Getenv("LITH_BENCH_KEYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1_000_000
}

// makeEntries builds n synthetic entries under a single "big/" prefix.
func makeEntries(n int) []Entry {
	e := make([]Entry, n)
	for i := range e {
		e[i] = Entry{Key: fmt.Sprintf("big/%09d", i), Size: int64(i), MTime: int64(i), ETagHash: uint64(i)}
	}
	return e
}

// BenchmarkBuild measures index construction throughput and reports the
// amortized bytes-per-key of both the in-memory arrays and the on-disk image.
func BenchmarkBuild(b *testing.B) {
	n := benchKeys()
	entries := makeEntries(n)

	b.ResetTimer()
	var ix *Index
	for i := 0; i < b.N; i++ {
		// Build consumes/reorders its input; give it a fresh copy each time.
		cp := make([]Entry, len(entries))
		copy(cp, entries)
		ix = Build(cp, Options{Bucket: "b"})
	}
	b.StopTimer()

	inMem := len(ix.arena) + len(ix.offs)*4 + ix.Len()*(8+8+8+8)
	onDisk := len(ix.Marshal())
	b.ReportMetric(float64(inMem)/float64(n), "memB/key")
	b.ReportMetric(float64(onDisk)/float64(n), "diskB/key")
	b.ReportMetric(float64(n), "keys")
}

// BenchmarkReaddirFull measures the latency of fully enumerating a prefix with
// benchKeys() immediate children, paging 4096 at a time.
func BenchmarkReaddirFull(b *testing.B) {
	n := benchKeys()
	ix := Build(makeEntries(n), Options{Bucket: "b"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var count, cursor uint64
		for {
			ents, next, err := ix.Readdir("/big", cursor, 4096)
			if err != nil {
				b.Fatal(err)
			}
			if len(ents) == 0 {
				break
			}
			count += uint64(len(ents))
			cursor = next
		}
		if count != uint64(n) {
			b.Fatalf("enumerated %d, want %d", count, n)
		}
	}
}

// BenchmarkLookup measures single-key lookup latency in a large index.
func BenchmarkLookup(b *testing.B) {
	n := benchKeys()
	ix := Build(makeEntries(n), Options{Bucket: "b"})
	key := fmt.Sprintf("/big/%09d", n/2)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.Stat(key); err != nil {
			b.Fatal(err)
		}
	}
}
