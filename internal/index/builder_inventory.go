// SPDX-License-Identifier: Apache-2.0

package index

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

// ParseManifest decodes an S3 Inventory manifest.json.
func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("inventory: parse manifest: %w", err)
	}
	return m, nil
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
		entries, err := readInventoryFile(ctx, opts, cols, root, f.Key)
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
	return Build(all, bopts), nil
}

func readInventoryFile(ctx context.Context, opts InventoryOptions, cols schemaColumns, root, fileKey string) ([]Entry, error) {
	rc, err := opts.Open(ctx, fileKey)
	if err != nil {
		return nil, fmt.Errorf("inventory: open %q: %w", fileKey, err)
	}
	defer func() { _ = rc.Close() }()

	var r io.Reader = rc
	if opts.GzipData {
		gz, err := gzip.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("inventory: gunzip %q: %w", fileKey, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
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
		rawKey := rec[cols.key]
		key, uerr := url.QueryUnescape(rawKey)
		if uerr != nil {
			key = rawKey // fall back to the raw value
		}
		rel := strings.TrimPrefix(key, root)

		e := Entry{Key: rel}
		if cols.size >= 0 && cols.size < len(rec) {
			if n, perr := strconv.ParseInt(strings.TrimSpace(rec[cols.size]), 10, 64); perr == nil {
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
