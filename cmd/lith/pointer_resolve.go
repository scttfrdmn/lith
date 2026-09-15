// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/pointer"
	"github.com/scttfrdmn/lith/internal/s3client"
)

// splitPointerRef splits a trailing "@ref" off a dataset prefix. It returns the
// dataset prefix and the ref: "current", a pinned version id, or "" when the URL
// carries no "@" (a plain prefix that mounts exactly as before).
func splitPointerRef(prefix string) (dataset, ref string) {
	if i := strings.LastIndexByte(prefix, '@'); i >= 0 {
		return prefix[:i], prefix[i+1:]
	}
	return prefix, ""
}

// resolvePointer resolves an @ref to a loaded, verified lith index for a
// published dataset (cargoship#560). It fails CLOSED (#141): every failure
// returns an error naming the offending key, and it issues only GETs — never a
// ListObjectsV2 — so a malformed or dangling pointer can never degrade into a
// whole-bucket listing. ref is "current" (via the CURRENT pointer object) or a
// pinned version id (resolved directly to that version's index). Returns the
// index and the resolved version id.
func resolvePointer(ctx context.Context, client s3client.API, bucket, dataset, ref string) (*index.Index, string, error) {
	dataset = strings.Trim(dataset, "/")
	var indexKey, wantSHA, versionID string

	switch ref {
	case "current":
		curKey := path.Join(dataset, "CURRENT")
		data, err := readPointerObject(ctx, client, curKey)
		if err != nil {
			return nil, "", fmt.Errorf("resolve @current: %w", err)
		}
		cur, err := pointer.Parse(data)
		if err != nil {
			return nil, "", fmt.Errorf("resolve @current from %q: %w", curKey, err)
		}
		indexKey, wantSHA, versionID = cur.IndexKey, cur.IndexSHA256, cur.VersionID
	default:
		if ref == "" {
			return nil, "", fmt.Errorf("resolve pointer: empty ref")
		}
		versionID = ref
		indexKey = path.Join(dataset, "v", ref, "index.lith")
	}

	img, _, err := client.GetRange(ctx, indexKey, 0, 0)
	if err != nil {
		return nil, "", fmt.Errorf("resolve @%s: GET index %q: %w", ref, indexKey, err)
	}
	if wantSHA != "" {
		got := hex.EncodeToString(sha256Sum(img))
		if got != wantSHA {
			return nil, "", fmt.Errorf("resolve @current: index %q sha256 %s does not match CURRENT %s", indexKey, got, wantSHA)
		}
	}
	ix, err := index.Unmarshal(img)
	if err != nil {
		return nil, "", fmt.Errorf("resolve @%s: load index %q: %w", ref, indexKey, err)
	}
	// Chunkless-manifest / empty-namespace guard (same class as #141): a published
	// index is always backed by a framed archive with at least one file. An index
	// built from a chunkless (direct-upload) manifest is rejected at build time by
	// cargoship.Parse; this is the reader-side backstop — an empty index would mount
	// an empty namespace, so fail closed and say why rather than mounting nothing.
	if ix.Len() == 0 {
		return nil, "", fmt.Errorf("resolve @%s: index %q is empty (a chunkless manifest yields no files) — refusing to mount an empty namespace", ref, indexKey)
	}
	return ix, versionID, nil
}

// readPointerObject fetches a CURRENT pointer object bounded AT THE NETWORK: it
// requests one byte past MaxPointerBytes, so a hostile or corrupt pointer is
// never fully read into memory (the old code read the whole object, then let
// pointer.Parse reject it after the fact — the "refused unread" property was not
// true in transit). An object at or over the cap is refused, naming the key and
// the cap (#195).
func readPointerObject(ctx context.Context, client s3client.API, key string) ([]byte, error) {
	data, _, err := client.GetRange(ctx, key, 0, pointer.MaxPointerBytes+1)
	if err != nil {
		return nil, fmt.Errorf("GET pointer %q: %w", key, err)
	}
	if len(data) > pointer.MaxPointerBytes {
		return nil, fmt.Errorf("pointer %q exceeds the %d-byte cap (read bounded at the network)", key, pointer.MaxPointerBytes)
	}
	return data, nil
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
