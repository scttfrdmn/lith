// SPDX-License-Identifier: Apache-2.0

package index

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Size/count ceilings that bound an inventory build. Both the manifest and the
// referenced data files are attacker-controllable in lith's threat model, so a
// gzip bomb or a file with billions of rows could otherwise expand into
// unbounded resident memory and OOM the process. These are declared as package
// vars (not consts) purely so tests can lower them; production never mutates
// them.
var (
	// maxManifestBytes caps the manifest.json JSON body. A real S3 Inventory
	// manifest is a few KiB; 256 MiB is a generous ceiling that still refuses a
	// hostile multi-gigabyte document.
	maxManifestBytes int64 = 256 << 20 // 256 MiB
	// maxDecompressedBytes caps the *decompressed* bytes read from a single
	// inventory data file, defeating gzip bombs. 8 GiB comfortably exceeds any
	// legitimate CSV inventory shard.
	maxDecompressedBytes int64 = 8 << 30 // 8 GiB
	// maxInventoryEntries caps the total rows accumulated across the whole
	// build so a file with billions of rows cannot exhaust memory.
	maxInventoryEntries = 500_000_000
)

// ErrTooManyInventoryEntries is returned when a build's accumulated rows exceed
// maxInventoryEntries.
var ErrTooManyInventoryEntries = errors.New("inventory: too many entries; refusing to index a possibly-hostile inventory")

// Manifest is the subset of an S3 Inventory manifest.json that lith reads.
type Manifest struct {
	SourceBucket string         `json:"sourceBucket"`
	FileFormat   string         `json:"fileFormat"` // "CSV", "Parquet", or "ORC"
	FileSchema   string         `json:"fileSchema"` // e.g. "Bucket, Key, Size, LastModifiedDate, ETag"
	Files        []ManifestFile `json:"files"`
}

// ManifestFile is one inventory data file referenced by the manifest.
type ManifestFile struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// ParseManifest decodes an S3 Inventory manifest.json. The reader is bounded by
// maxManifestBytes so a hostile manifest cannot exhaust memory during decode.
func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	capped := &cappedReader{
		r:   r,
		n:   maxManifestBytes + 1, // +1 so a body of exactly the cap still fits
		err: fmt.Errorf("inventory: manifest exceeds the size limit (%d bytes) — refusing to parse a possibly-hostile manifest", maxManifestBytes),
	}
	if err := json.NewDecoder(capped).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("inventory: parse manifest: %w", err)
	}
	return m, nil
}

// cappedReader wraps an io.Reader and returns err once more than the configured
// number of bytes have been read. It is initialized with n = cap+1: a stream of
// exactly cap bytes reaches EOF naturally (n never hits 0), while a stream that
// yields cap+1 bytes trips the limit. This lets a downstream csv/json reader
// surface a clear error instead of silently seeing a truncated stream.
type cappedReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, c.err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	if c.n <= 0 && err == nil {
		// Consumed cap+1 bytes without hitting EOF: over the limit.
		err = c.err
	}
	return n, err
}

// InventoryOptions configures building an index from an S3 Inventory manifest.
type InventoryOptions struct {
	Options
	// Manifest is the parsed inventory manifest.
	Manifest Manifest
	// Open fetches an inventory data file by its key (relative to the
	// inventory destination bucket). The command layer wires this to S3 or a
	// local directory.
	Open func(ctx context.Context, key string) (io.ReadCloser, error)
	// GzipData indicates the CSV data files are gzip-compressed (the S3
	// default). Set false for plain CSV (used by tests).
	GzipData bool
}

// schemaColumns maps the inventory fileSchema to column indexes.
type schemaColumns struct {
	key, size, lastModified, etag int
}

func parseSchema(schema string) schemaColumns {
	cols := schemaColumns{key: -1, size: -1, lastModified: -1, etag: -1}
	for i, name := range strings.Split(schema, ",") {
		switch strings.TrimSpace(name) {
		case "Key":
			cols.key = i
		case "Size":
			cols.size = i
		case "LastModifiedDate":
			cols.lastModified = i
		case "ETag":
			cols.etag = i
		}
	}
	return cols
}

// BuildFromInventory builds an index from the data files referenced by an S3
// Inventory manifest. Only the CSV format is supported; Parquet and ORC return
// an error (see issue #10).
func BuildFromInventory(ctx context.Context, opts InventoryOptions) (*Index, error) {
	switch strings.ToUpper(opts.Manifest.FileFormat) {
	case "CSV":
		// supported
	case "PARQUET", "ORC":
		return nil, fmt.Errorf("inventory: %s format is not yet supported; regenerate the inventory as CSV (see issue #10)", opts.Manifest.FileFormat)
	default:
		return nil, fmt.Errorf("inventory: unknown fileFormat %q", opts.Manifest.FileFormat)
	}
	if opts.Open == nil {
		return nil, fmt.Errorf("inventory: Open function is required")
	}

	cols := parseSchema(opts.Manifest.FileSchema)
	if cols.key < 0 {
		return nil, fmt.Errorf("inventory: fileSchema %q has no Key column", opts.Manifest.FileSchema)
	}

	root := normalizePrefix(opts.Prefix)
	var all []Entry
	for _, f := range opts.Manifest.Files {
		// Bound each file's rows by the remaining global budget so a single
		// billion-row file cannot exhaust memory before we return.
		remaining := maxInventoryEntries - len(all)
		entries, err := readInventoryFile(ctx, opts, cols, root, f.Key, remaining)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
		if opts.Logger != nil {
			opts.Logger.Info("inventory file ingested", "file", f.Key, "rows", len(entries))
		}
	}

	bopts := opts.Options
	bopts.Prefix = root
	bopts.Source = "inventory"
	return Build(all, bopts), nil
}

func readInventoryFile(ctx context.Context, opts InventoryOptions, cols schemaColumns, root, fileKey string, maxEntries int) ([]Entry, error) {
	rc, err := opts.Open(ctx, fileKey)
	if err != nil {
		return nil, fmt.Errorf("inventory: open %q: %w", fileKey, err)
	}
	defer func() { _ = rc.Close() }()

	// Bound the raw object read (a compressed gzip bomb is small on the wire,
	// but a plain-CSV data file could itself be enormous) and, separately, the
	// decompressed byte count.
	var r io.Reader = &cappedReader{
		r:   rc,
		n:   maxDecompressedBytes + 1,
		err: fmt.Errorf("inventory: %q exceeds the size limit (%d bytes) — refusing to index a possibly-hostile file", fileKey, maxDecompressedBytes),
	}
	if opts.GzipData {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("inventory: gunzip %q: %w", fileKey, err)
		}
		defer func() { _ = gz.Close() }()
		// Cap the decompressed output to defeat gzip bombs.
		r = &cappedReader{
			r:   gz,
			n:   maxDecompressedBytes + 1,
			err: fmt.Errorf("inventory: %q exceeds the decompressed-size limit (%d bytes) — refusing to index a possibly-hostile file", fileKey, maxDecompressedBytes),
		}
	}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // inventory schemas vary in width
	cr.ReuseRecord = true

	var out []Entry
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inventory: read %q: %w", fileKey, err)
		}
		if cols.key >= len(rec) {
			continue
		}
		if len(out) >= maxEntries {
			return nil, ErrTooManyInventoryEntries
		}
		rawKey := rec[cols.key]
		// PathUnescape (not QueryUnescape): S3 keys are percent-encoded, and a
		// literal '+' in a key must stay '+' rather than decode to a space.
		key, uerr := url.PathUnescape(rawKey)
		if uerr != nil {
			key = rawKey // fall back to the raw value
		}
		rel := strings.TrimPrefix(key, root)

		e := Entry{Key: rel}
		if cols.size >= 0 && cols.size < len(rec) {
			// Reject a negative size (never valid for an object); leave Size at
			// 0, matching how unparseable sizes are already treated.
			if n, perr := strconv.ParseInt(strings.TrimSpace(rec[cols.size]), 10, 64); perr == nil && n >= 0 {
				e.Size = n
			}
		}
		if cols.lastModified >= 0 && cols.lastModified < len(rec) {
			if ts, perr := parseInventoryTime(rec[cols.lastModified]); perr == nil {
				e.MTime = ts.UnixNano()
			}
		}
		if cols.etag >= 0 && cols.etag < len(rec) {
			e.ETagHash = HashETag(rec[cols.etag])
		}
		out = append(out, e)
	}
	return out, nil
}

func parseInventoryTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	// S3 Inventory uses ISO-8601, commonly with milliseconds and a Z.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("inventory: unrecognized time %q", s)
}
