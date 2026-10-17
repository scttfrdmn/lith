// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// doctor grades credentials by whether they SURVIVE a long mount, not merely whether they
// resolve. Before, any credential that resolved was a pass -- including exported STS credentials
// that would fail the mount within hours.
func TestDoctorGradesCredentialsBySurvival(t *testing.T) {
	cases := []struct {
		name string
		c    aws.Credentials
		want checkStatus
		has  string
	}{
		{"env session token: cannot be renewed", aws.Credentials{AccessKeyID: "ASIA", SessionToken: "t", Source: "EnvConfigCredentials"}, warn, "TEMPORARY but STATIC"},
		{"credentials file session token: re-read by lith", aws.Credentials{AccessKeyID: "ASIA", SessionToken: "t", Source: "SharedConfigCredentials: /home/u/.aws/credentials"}, pass, "re-reads the credentials file"},
		{"instance role: renews itself", aws.Credentials{AccessKeyID: "ASIA", SessionToken: "t", CanExpire: true, Expires: time.Unix(2_000_000_000, 0), Source: "EC2RoleProvider"}, pass, "renews itself"},
		{"long-term key", aws.Credentials{AccessKeyID: "AKIA", Source: "EnvConfigCredentials"}, pass, "does not expire"},
	}
	for _, c := range cases {
		st, name, detail, fix := credentialVerdict(c.c)
		if st != c.want {
			t.Errorf("%s: status %v, want %v (%s)", c.name, st, c.want, detail)
		}
		if name != "credentials" {
			t.Errorf("%s: check named %q", c.name, name)
		}
		if !strings.Contains(detail, c.has) {
			t.Errorf("%s: detail %q does not contain %q", c.name, detail, c.has)
		}
		if st == warn && fix == "" {
			t.Errorf("%s: a warning with no remedy", c.name)
		}
	}
}
