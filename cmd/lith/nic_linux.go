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

// ethtoolGbps returns the negotiated link speed in Gbps via ethtool, or 0. ENA
// interfaces usually report "Unknown!", so this is only the first of several
// sources tried by resolveNIC (see nic.go).
func ethtoolGbps() float64 {
	iface := defaultRouteIface()
	if iface == "" {
		return 0
	}
	// Resolve ethtool against a fixed trusted dir list (not $PATH): under `sudo`
	// without secure_path a poisoned PATH would otherwise run an attacker binary
	// as root (finding F5). Not found → behave as the existing failure path.
	bin, err := trustedExecPath("ethtool")
	if err != nil {
		return 0
	}
	out, err := exec.Command(bin, iface).CombinedOutput()
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
