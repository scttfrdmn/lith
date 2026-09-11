// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestParseKeyList(t *testing.T) {
	in := strings.Join([]string{
		"# a comment",
		"",
		"  plain/key  ",
		"sized/key\t123",
		"full/key\t456\t1700000000000000000",
		"\t# whitespace then comment-like (still a key line? no—leading tab, key empty)",
	}, "\n")
	got, err := ParseKeyList(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseKeyList: %v", err)
	}
	// The last line: "\t# ..." splits on tab -> fields[0]=="" -> skipped.
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Key != "plain/key" || got[0].HasMeta {
		t.Errorf("entry0 = %+v", got[0])
	}
	if got[1].Key != "sized/key" || got[1].Size != 123 || got[1].HasMeta {
		t.Errorf("entry1 = %+v (size-only must not set HasMeta)", got[1])
	}
	if got[2].Key != "full/key" || got[2].Size != 456 || got[2].MTime != 1700000000000000000 || !got[2].HasMeta {
		t.Errorf("entry2 = %+v", got[2])
	}
}

func TestParseKeyListMalformed(t *testing.T) {
	cases := []struct{ name, in, wantLine string }{
		{"bad size", "ok/a\nbad/b\tnotanumber", "line 2"},
		{"bad mtime", "ok/a\nbad/b\t10\tnope", "line 2"},
		{"negative size", "bad\t-5", "line 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKeyList(strings.NewReader(tc.in))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantLine) {
				t.Errorf("error %q should name %q", err, tc.wantLine)
			}
		})
	}
}

func TestMaybeGunzipPlain(t *testing.T) {
	r, err := MaybeGunzip(strings.NewReader("a/b\nc/d\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	if string(b) != "a/b\nc/d\n" {
		t.Errorf("plain round-trip = %q", b)
	}
}

func TestMaybeGunzipGz(t *testing.T) {
	const payload = "one/key\ntwo/key\t9\nthree/key\t9\t42\n"
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := MaybeGunzip(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseKeyList(r)
	if err != nil {
		t.Fatalf("ParseKeyList after gunzip: %v", err)
	}
	if len(keys) != 3 || keys[0].Key != "one/key" || !keys[2].HasMeta {
		t.Errorf("gz round-trip = %+v", keys)
	}
}
