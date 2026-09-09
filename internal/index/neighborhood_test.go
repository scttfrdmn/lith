// SPDX-License-Identifier: Apache-2.0

package index

import (
	"reflect"
	"testing"
)

// TestNeighborhoodDirectChildren: siblings are the following direct children of
// the same directory, in key order, and keys inside a subdirectory are skipped.
func TestNeighborhoodDirectChildren(t *testing.T) {
	ix := build("",
		"d/a", "d/b", "d/c",
		"d/sub/x", "d/sub/y", // a subdirectory between c and z
		"d/z",
		"e/f", // a different directory
	)

	got := ix.Neighborhood("/d/a", 10)
	var keys []string
	for _, s := range got {
		keys = append(keys, s.Key)
	}
	// After d/a, still under d/: d/b, d/c, (skip d/sub/*), d/z. e/f is out.
	want := []string{"d/b", "d/c", "d/z"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("Neighborhood(d/a) keys = %v, want %v", keys, want)
	}
}

func TestNeighborhoodRespectsLimit(t *testing.T) {
	ix := build("", "d/0", "d/1", "d/2", "d/3", "d/4")
	got := ix.Neighborhood("/d/0", 2)
	if len(got) != 2 || got[0].Key != "d/1" || got[1].Key != "d/2" {
		t.Fatalf("Neighborhood(d/0, 2) = %+v", got)
	}
	if got[0].Size == 0 || got[0].ETagHash == 0 {
		t.Fatalf("sibling metadata not populated: %+v", got[0])
	}
}

func TestNeighborhoodZeroAndEnd(t *testing.T) {
	ix := build("", "d/0", "d/1")
	if got := ix.Neighborhood("/d/0", 0); got != nil {
		t.Fatalf("n=0 returned %v, want nil", got)
	}
	// Last key in its directory: no following siblings.
	if got := ix.Neighborhood("/d/1", 5); len(got) != 0 {
		t.Fatalf("Neighborhood(last) = %v, want empty", got)
	}
}

// TestNeighborhoodWithPrefix: with a mount prefix, returned keys are still
// Index-relative (prefix stripped), so Prefix()+Key reconstructs the object key.
func TestNeighborhoodWithPrefix(t *testing.T) {
	ix := build("root/", "d/0", "d/1", "d/2")
	got := ix.Neighborhood("/d/0", 5)
	if len(got) != 2 || got[0].Key != "d/1" {
		t.Fatalf("Neighborhood with prefix = %+v", got)
	}
}
