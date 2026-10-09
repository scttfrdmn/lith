// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var ethtoolSpeed = regexp.MustCompile(`Speed:\s*(\d+)Mb/s`)

// ethtoolGbps returns the negotiated link speed in Gbps via ethtool, or 0 with a reason
// (#317). ENA interfaces usually report "Unknown!", so this is only the first of several
// sources tried by resolveNIC (see nic.go).
//
// The reason is returned because the four ways this fails call for four different operator
// actions -- install ethtool, fix the default route, accept that ENA has no speed to report,
// or look at a parse failure -- and they were indistinguishable when the function returned a
// bare 0. On the box in #317 it is the third, which is both the common case on EC2 and the
// one where nothing is wrong and nothing can be done.
func ethtoolGbps() (float64, string) {
	iface := defaultRouteIface()
	if iface == "" {
		return 0, "no default-route interface in /proc/net/route"
	}
	// Resolve ethtool against a fixed trusted dir list (not $PATH): under `sudo`
	// without secure_path a poisoned PATH would otherwise run an attacker binary
	// as root (finding F5). Not found → behave as the existing failure path.
	bin, err := trustedExecPath("ethtool")
	if err != nil {
		return 0, "ethtool not found in a trusted directory"
	}
	out, err := exec.Command(bin, iface).CombinedOutput()
	if err != nil {
		return 0, fmt.Sprintf("ethtool %s failed: %v", iface, err)
	}
	m := ethtoolSpeed.FindStringSubmatch(string(out))
	if m == nil {
		return 0, fmt.Sprintf("%s reports no link speed (normal for ENA, which answers "+
			"\"Unknown!\")", iface)
	}
	mbps, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Sprintf("could not parse %q as a link speed", m[1])
	}
	return mbps / 1000, ""
}

// defaultRouteIface reads the interface with the default route from
// /proc/net/route (destination 00000000).
func defaultRouteIface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}
