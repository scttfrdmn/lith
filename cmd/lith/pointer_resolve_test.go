// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/scttfrdmn/lith/internal/pointer"
	"github.com/scttfrdmn/lith/internal/s3client"
)

// recordingClient records the GetRange window it was asked for and serves at
// most `length` bytes of body, like a real ranged GET.
type recordingClient struct {
	s3client.API
	body   []byte
	gotOff int64
	gotLen int64
}

func (c *recordingClient) GetRange(_ context.Context, _ string, off, length int64) ([]byte, string, error) {
	c.gotOff, c.gotLen = off, length
	b := c.body
	if length > 0 && int64(len(b)) > length {
		b = b[:length]
	}
	return b, "etag", nil
}

// TestReadPointerObjectBoundsTheRead asserts the CURRENT read is bounded at the
// network (#195): an oversized pointer is rejected AND only MaxPointerBytes+1
// bytes are ever requested — the read itself is bounded, not just Parse's
// after-the-fact length check.
func TestReadPointerObjectBoundsTheRead(t *testing.T) {
	// A hostile CURRENT far larger than the cap.
	big := &recordingClient{body: bytes.Repeat([]byte("x"), pointer.MaxPointerBytes*4)}
	_, err := readPointerObject(context.Background(), big, "d/CURRENT")
	if err == nil {
		t.Fatal("expected oversized CURRENT to be rejected")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
	// The assertion that matters: the read was bounded, not the whole 256 KiB.
	if big.gotLen != pointer.MaxPointerBytes+1 {
		t.Errorf("requested %d bytes, want the bounded %d (MaxPointerBytes+1)", big.gotLen, pointer.MaxPointerBytes+1)
	}
	if big.gotOff != 0 {
		t.Errorf("requested offset %d, want 0", big.gotOff)
	}

	// A well-formed small pointer is returned intact.
	small := &recordingClient{body: []byte("cargoship-pointer-v1\n")}
	data, err := readPointerObject(context.Background(), small, "d/CURRENT")
	if err != nil {
		t.Fatalf("small pointer: %v", err)
	}
	if !bytes.Equal(data, small.body) {
		t.Errorf("small pointer body = %q, want %q", data, small.body)
	}
}
