// SPDX-License-Identifier: Apache-2.0

// Package pointer parses the CURRENT pointer object of a published dataset
// (cargoship#560): a small JSON object naming the current immutable version's
// index and manifest. lith does not own the bucket the pointer lives in, so its
// bytes are treated as hostile (#101): a hard size cap, every field validated
// before use, and no input may cause a panic. On any malformed input Parse
// returns a non-nil error naming the reason; callers fail closed (#141) — they
// must never fall back to listing the bucket.
package pointer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Schema is the only CURRENT schema this version understands. A pointer with any
// other schema is rejected rather than guessed at.
const Schema = "cargoship.current/1"

// MaxPointerBytes caps the CURRENT object. It is a handful of short fields; a
// pointer larger than this is malformed or hostile and is refused unread.
const MaxPointerBytes = 64 << 10

// Current is a parsed, validated CURRENT pointer.
type Current struct {
	Schema            string `json:"schema"`
	VersionID         string `json:"version_id"`
	ManifestKey       string `json:"manifest_key"`
	IndexKey          string `json:"index_key"`
	IndexSHA256       string `json:"index_sha256"`
	Created           string `json:"created"`
	FileCount         int64  `json:"file_count"`
	TotalBytes        int64  `json:"total_bytes"`
	PreviousVersionID string `json:"previous_version_id"`
}

// Parse validates untrusted CURRENT bytes and returns the pointer. It never
// panics; every failure is a returned error. The size cap is checked before the
// JSON decoder runs so a hostile object cannot force a large allocation.
func Parse(b []byte) (*Current, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("CURRENT is empty")
	}
	if len(b) > MaxPointerBytes {
		return nil, fmt.Errorf("CURRENT is %d bytes, exceeds the %d-byte cap", len(b), MaxPointerBytes)
	}
	var c Current
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("CURRENT is not valid JSON: %w", err)
	}
	// Reject trailing garbage after the object.
	if dec.More() {
		return nil, fmt.Errorf("CURRENT has trailing data after the JSON object")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate enforces the required fields and their shapes. A pointer that passes
// validate names an index and a manifest a reader can resolve.
func (c *Current) validate() error {
	if c.Schema != Schema {
		return fmt.Errorf("CURRENT schema %q is not supported (want %q)", c.Schema, Schema)
	}
	if c.VersionID == "" {
		return fmt.Errorf("CURRENT has no version_id")
	}
	if c.ManifestKey == "" {
		return fmt.Errorf("CURRENT has no manifest_key")
	}
	if c.IndexKey == "" {
		return fmt.Errorf("CURRENT has no index_key")
	}
	// index_sha256 is optional-empty tolerated? No — a pointer that names an index
	// must commit to its bytes so the reader can verify what it loaded.
	if len(c.IndexSHA256) != 64 {
		return fmt.Errorf("CURRENT index_sha256 must be 64 hex chars, got %d", len(c.IndexSHA256))
	}
	if _, err := hex.DecodeString(c.IndexSHA256); err != nil {
		return fmt.Errorf("CURRENT index_sha256 is not hex: %w", err)
	}
	if c.FileCount < 0 {
		return fmt.Errorf("CURRENT file_count is negative")
	}
	if c.TotalBytes < 0 {
		return fmt.Errorf("CURRENT total_bytes is negative")
	}
	return nil
}
