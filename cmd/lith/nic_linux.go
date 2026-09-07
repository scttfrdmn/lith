// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ethtoolSpeed = regexp.MustCompile(`Speed:\s*(\d+)Mb/s`)

// nicGbps returns the NIC bandwidth in Gbps, or 0 if unknown. It tries ethtool
// first (which ENA usually reports as "Unknown!"), then the EC2 instance-type
// table via IMDS.
func nicGbps() float64 {
	if g := ethtoolGbps(); g > 0 {
		return g
	}
	return imdsInstanceGbps()
}

func ethtoolGbps() float64 {
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

// instanceGbps maps EC2 instance types to advertised (peak) network Gbps. Used
// as the NIC fallback when ethtool cannot report the link speed. Extend as
// needed; unknown types fall back to the 512 MiB budget constant.
var instanceGbps = map[string]float64{
	"c8gd.large": 12.5, "c8gd.xlarge": 12.5, "c8gd.2xlarge": 15, "c8gd.4xlarge": 15,
	"c8gd.8xlarge": 15, "c8gd.12xlarge": 22.5, "c8gd.16xlarge": 30, "c8gd.24xlarge": 40, "c8gd.48xlarge": 50,
	"c7gd.large": 12.5, "c7gd.xlarge": 12.5, "c7gd.2xlarge": 15, "c7gd.4xlarge": 15,
	"c7gd.8xlarge": 15, "c7gd.12xlarge": 22.5, "c7gd.16xlarge": 30,
}

// imdsInstanceGbps reads the instance type from IMDSv2 and looks it up.
func imdsInstanceGbps() float64 {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	tokReq, _ := http.NewRequest(http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return 0
	}
	tok, _ := io.ReadAll(tokResp.Body)
	_ = tokResp.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/instance-type", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return instanceGbps[strings.TrimSpace(string(b))]
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
