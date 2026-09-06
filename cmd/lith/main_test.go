// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func TestParseS3URL(t *testing.T) {
	cases := []struct {
		in      string
		bucket  string
		prefix  string
		wantErr bool
	}{
		{"s3://bucket", "bucket", "", false},
		{"s3://bucket/prefix", "bucket", "prefix", false},
		{"s3://bucket/a/b/c", "bucket", "a/b/c", false},
		{"bucket/prefix", "", "", true},
		{"s3://", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range cases {
		b, p, err := parseS3URL(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseS3URL(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && (b != tc.bucket || p != tc.prefix) {
			t.Errorf("parseS3URL(%q) = %q,%q; want %q,%q", tc.in, b, p, tc.bucket, tc.prefix)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "lith ") || !strings.Contains(buf.String(), "commit:") {
		t.Errorf("version output = %q", buf.String())
	}
}

func TestMountAndBenchNotImplemented(t *testing.T) {
	for _, sub := range []string{"mount", "bench"} {
		root := newRootCmd()
		root.SetArgs([]string{sub})
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("%s: err = %v, want 'not implemented'", sub, err)
		}
	}
}

// TestIndexBuildAndInspect drives `index build` against the fake, then
// `index inspect` over the written file — all network-free via the client seam.
func TestIndexBuildAndInspect(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("data/a", "AAA", now)
	srv.PutString("data/b/c", "CC", now)
	srv.PutString("data/bad//x", "oops", now)

	orig := newS3Client
	newS3Client = func(context.Context, s3client.Config) (s3client.API, error) { return srv, nil }
	defer func() { newS3Client = orig }()

	idxPath := t.TempDir() + "/t.lithidx"

	// build
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"index", "build", "s3://bkt/data", "--index-file", idxPath, "--no-sign-request"})
	if err := root.Execute(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(out.String(), "2 keys") || !strings.Contains(out.String(), "dropped 1") {
		t.Errorf("build output = %q", out.String())
	}

	// inspect
	root = newRootCmd()
	out.Reset()
	root.SetOut(&out)
	root.SetArgs([]string{"index", "inspect", idxPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	s := out.String()
	for _, want := range []string{"bucket:            bkt", "prefix:            data/", "keys:              2", "dropped-keys:      1"} {
		if !strings.Contains(s, want) {
			t.Errorf("inspect output missing %q in:\n%s", want, s)
		}
	}
}
