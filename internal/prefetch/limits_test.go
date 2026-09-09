// SPDX-License-Identifier: Apache-2.0

package prefetch

import (
	"sync"
	"testing"
)

func TestPolicyReserveRelease(t *testing.T) {
	p := NewPolicy(100, DeviceLimits{}, nil)

	if total, used := p.Budget(); total != 100 || used != 0 {
		t.Fatalf("initial Budget() = (%d,%d), want (100,0)", total, used)
	}
	if !p.Reserve(60) {
		t.Fatal("Reserve(60) failed on empty budget")
	}
	if _, used := p.Budget(); used != 60 {
		t.Fatalf("used = %d, want 60", used)
	}
	// 60 + 50 > 100: must fail and not change the reservation.
	if p.Reserve(50) {
		t.Fatal("Reserve(50) succeeded past the cap")
	}
	if _, used := p.Budget(); used != 60 {
		t.Fatalf("used = %d after failed reserve, want 60", used)
	}
	// Exactly to the cap succeeds.
	if !p.Reserve(40) {
		t.Fatal("Reserve(40) to the cap failed")
	}
	if _, used := p.Budget(); used != 100 {
		t.Fatalf("used = %d, want 100 (full)", used)
	}
	p.Release(100)
	if _, used := p.Budget(); used != 0 {
		t.Fatalf("used = %d after full release, want 0", used)
	}
}

func TestPolicyReserveNonPositive(t *testing.T) {
	p := NewPolicy(10, DeviceLimits{}, nil)
	if !p.Reserve(0) {
		t.Fatal("Reserve(0) should always succeed")
	}
	if _, used := p.Budget(); used != 0 {
		t.Fatalf("Reserve(0) reserved %d bytes, want 0", used)
	}
}

// TestPolicyReleaseClampsAtZero: an over-release cannot drive the counter
// negative and thus cannot fabricate budget.
func TestPolicyReleaseClampsAtZero(t *testing.T) {
	p := NewPolicy(100, DeviceLimits{}, nil)
	p.Reserve(10)
	p.Release(50) // release more than reserved
	if _, used := p.Budget(); used != 0 {
		t.Fatalf("used = %d after over-release, want 0", used)
	}
	// The full budget is still available.
	if !p.Reserve(100) {
		t.Fatal("Reserve(100) failed after over-release corrupted the counter")
	}
}

// TestPolicyReserveDisabled: a non-positive cap disables reservations.
func TestPolicyReserveDisabled(t *testing.T) {
	p := NewPolicy(0, DeviceLimits{}, nil)
	if p.Reserve(1) {
		t.Fatal("Reserve(1) should fail when the budget is disabled")
	}
}

// TestPolicyReserveConcurrent: under concurrent reservers the total never
// exceeds the cap.
func TestPolicyReserveConcurrent(t *testing.T) {
	const cap = 1000
	p := NewPolicy(cap, DeviceLimits{}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if p.Reserve(10) {
					if _, used := p.Budget(); used > cap {
						t.Errorf("used %d exceeded cap %d", used, cap)
					}
					p.Release(10)
				}
			}
		}()
	}
	wg.Wait()
	if _, used := p.Budget(); used != 0 {
		t.Fatalf("used = %d after all releases, want 0", used)
	}
}

func TestPolicyNeighborhoodAndDevice(t *testing.T) {
	dev := DeviceLimits{NICBDPBytes: 1 << 20, MemCacheBytes: 8 << 20, DiskWriteBPS: 0}
	sibs := []Sibling{{Key: "d/1", Size: 5}, {Key: "d/2", Size: 6}}
	p := NewPolicy(10, dev, func(key string, n int) []Sibling {
		if key == "d/0" && n >= 2 {
			return sibs
		}
		return nil
	})
	if got := p.Neighborhood("d/0", 4); len(got) != 2 || got[1].Key != "d/2" {
		t.Fatalf("Neighborhood = %v", got)
	}
	if got := p.Device(); got != dev {
		t.Fatalf("Device = %+v, want %+v", got, dev)
	}
	// nil neigh returns nil.
	if got := NewPolicy(1, dev, nil).Neighborhood("x", 1); got != nil {
		t.Fatalf("nil-neigh Neighborhood = %v, want nil", got)
	}
}
