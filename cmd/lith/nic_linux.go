// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var ethtoolSpeed = regexp.MustCompile(`Speed:\s*(\d+)Mb/s`)

// nicGbps returns the primary interface's link speed in Gbps, or 0 if unknown
// (ENA often reports "Unknown!"). Best-effort via ethtool on the default-route
// interface.
func nicGbps() float64 {
	iface := defaultRouteIface()
	if iface == "" {
		return 0
	}
	out, err := exec.Command("ethtool", iface).CombinedOutput()
	if err != nil {
		return 0
	}
	m := ethtoolSpeed.FindStringSubmatch(string(out))
	if m == nil {
		return 0
	}
	mbps, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return mbps / 1000
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
