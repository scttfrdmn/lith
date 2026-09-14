// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasUncommented(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"user_allow_other\n", true},
		{"  user_allow_other  \n", true},
		{"# user_allow_other\n", false},
		{"#user_allow_other\n", false},
		{"mount_max = 1000\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hasUncommented(c.in, "user_allow_other"); got != c.want {
			t.Errorf("hasUncommented(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDoctorMountpointCheck(t *testing.T) {
	// helper: run doctorMountpoint against a path and return (failed, output)
	run := func(mp string) (bool, string) {
		var buf bytes.Buffer
		d := &doctor{out: &buf}
		doctorMountpoint(d, mp)
		return d.failed, buf.String()
	}

	t.Run("missing", func(t *testing.T) {
		failed, out := run(filepath.Join(t.TempDir(), "nope"))
		if !failed || !strings.Contains(out, "mkdir") {
			t.Errorf("missing mountpoint should FAIL with a mkdir fix; got %q", out)
		}
	})
	t.Run("not a dir", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		failed, out := run(f)
		if !failed || !strings.Contains(out, "not a directory") {
			t.Errorf("file mountpoint should FAIL; got %q", out)
		}
	})
	t.Run("non-empty", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		failed, out := run(dir)
		if !failed || !strings.Contains(out, "not empty") {
			t.Errorf("non-empty mountpoint should FAIL; got %q", out)
		}
	})
	t.Run("empty dir owned by us", func(t *testing.T) {
		failed, out := run(t.TempDir())
		if failed || !strings.Contains(out, "PASS") {
			t.Errorf("empty owned dir should PASS; got %q", out)
		}
	})
}

func TestDoctorFailAccumulates(t *testing.T) {
	var buf bytes.Buffer
	d := &doctor{out: &buf}
	d.add(pass, "a", "ok", "")
	if d.failed {
		t.Fatal("pass must not set failed")
	}
	d.add(na, "b", "n/a", "")
	if d.failed {
		t.Fatal("n/a must not set failed")
	}
	d.add(fail, "c", "bad", "do x")
	if !d.failed {
		t.Fatal("fail must set failed")
	}
	if !strings.Contains(buf.String(), "fix: do x") {
		t.Errorf("a fail must print its fix; got %q", buf.String())
	}
}
