// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// A malformed --inflight-bytes must FAIL, not be silently replaced by the derived default
// (#393). `serve nfs` always errored; `lith mount` only passed the string to
// computeInflightBytes, which falls through on a parse failure — so the flag was accepted,
// discarded, and the mount logged a derived budget with nothing saying the flag was ignored.
// The #264 class: a flag that looks set and is not.
func TestValidateInflightBytes(t *testing.T) {
	// parseSize is deliberately lenient -- case-insensitive, and it trims whitespace both
	// around the value and between the number and the unit -- so these are all valid by
	// design and this test must not pretend otherwise.
	for _, ok := range []string{"", "512MiB", "1GiB", "268435456", "64MB", "512 MiB", "512mib"} {
		if err := validateInflightBytes(ok); err != nil {
			t.Errorf("validateInflightBytes(%q) = %v, want nil", ok, err)
		}
	}
	// What is actually unusable: unparseable, zero, negative, an unknown unit, a bare unit.
	for _, bad := range []string{"lots", "0", "-1", "1XiB", "MiB", "  "} {
		err := validateInflightBytes(bad)
		if err == nil {
			t.Errorf("validateInflightBytes(%q) accepted a value that cannot be used; it "+
				"would be silently replaced by the NIC-derived default", bad)
			continue
		}
		// The message has to name the flag, or the operator cannot find it.
		if !strings.Contains(err.Error(), "--inflight-bytes") {
			t.Errorf("validateInflightBytes(%q) error does not name the flag: %v", bad, err)
		}
	}
	// AND THE TWO FAILURE MODES MUST BE DISTINGUISHABLE. parseSize returns (0, err) for an
	// unparseable value, so the non-positive check catches it too and the function still
	// errors -- behaviour preserved, message degraded. "must be positive" is the wrong
	// diagnosis for a typo, and a revert that removed the parse check stayed green until this
	// assertion existed.
	for _, typo := range []string{"lots", "1XiB", "MiB"} {
		err := validateInflightBytes(typo)
		if err == nil {
			t.Fatalf("validateInflightBytes(%q) accepted it", typo)
		}
		if strings.Contains(err.Error(), "must be positive") {
			t.Errorf("validateInflightBytes(%q) blamed positivity for a parse failure, which "+
				"sends the operator looking for the wrong mistake: %v", typo, err)
		}
		if !strings.Contains(err.Error(), "invalid size") {
			t.Errorf("validateInflightBytes(%q) does not report it as an invalid size: %v",
				typo, err)
		}
	}
	// And zero IS a positivity failure, reported as one.
	if err := validateInflightBytes("0"); err == nil ||
		!strings.Contains(err.Error(), "must be positive") {
		t.Errorf(`validateInflightBytes("0") = %v, want a positivity error`, err)
	}
	// And the valid values must agree with what the derivation actually uses, or validation
	// would pass something computeInflightBytes then discards.
	for _, v := range []string{"512MiB", "1GiB", "64MB"} {
		if err := validateInflightBytes(v); err != nil {
			t.Fatalf("%q rejected: %v", v, err)
		}
		n, desc := computeInflightBytes(v, 25)
		if n <= 0 || !strings.Contains(desc, "--inflight-bytes") {
			t.Errorf("%q validated but computeInflightBytes ignored it: n=%d desc=%q",
				v, n, desc)
		}
	}
}
