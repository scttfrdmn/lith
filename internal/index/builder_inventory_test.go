// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	const j = `{
		"sourceBucket": "src",
		"fileFormat": "CSV",
		"fileSchema": "Bucket, Key, Size, LastModifiedDate, ETag",
		"files": [{"key": "inv/data/part-0.csv.gz", "size": 42}]
	}`
	m, err := ParseManifest(strings.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	if m.FileFormat != "CSV" || len(m.Files) != 1 || m.Files[0].Key != "inv/data/part-0.csv.gz" {
		t.Fatalf("manifest = %+v", m)
	}
	cols := parseSchema(m.FileSchema)
	if cols.key != 1 || cols.size != 2 || cols.lastModified != 3 || cols.etag != 4 {
		t.Fatalf("columns = %+v", cols)
	}
}

func TestBuildFromInventoryCSV(t *testing.T) {
	// Bucket, Key, Size, LastModifiedDate, ETag ; keys are URL-encoded.
	csvData := "" +
		`"bkt","data/a.txt","3","2016-11-30T00:00:00.000Z","etag-a"` + "\n" +
		`"bkt","data/sub%2Ffile.txt","5","2016-11-30T01:02:03.000Z","etag-b"` + "\n" +
		`"bkt","data/dir/deep","7","2016-11-30T01:02:03.000Z","etag-c"` + "\n"

	files := map[string]string{"inv/part-0.csv": csvData}
	open := func(_ context.Context, key string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(files[key])), nil
	}
	ix, err := BuildFromInventory(context.Background(), InventoryOptions{
		Options: Options{Bucket: "bkt", Prefix: "data"},
		Manifest: Manifest{
			FileFormat: "CSV",
			FileSchema: "Bucket, Key, Size, LastModifiedDate, ETag",
			Files:      []ManifestFile{{Key: "inv/part-0.csv"}},
		},
		Open:     open,
		GzipData: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// data/sub%2Ffile.txt decodes to data/sub/file.txt -> relative sub/file.txt
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
	if fi, err := ix.Stat("/a.txt"); err != nil || fi.Size != 3 {
		t.Errorf("stat /a.txt: fi=%+v err=%v", fi, err)
	}
	if fi, err := ix.Stat("/sub/file.txt"); err != nil || fi.Size != 5 {
		t.Errorf("stat /sub/file.txt: fi=%+v err=%v", fi, err)
	}
	if fi, err := ix.Stat("/dir"); err != nil || !fi.IsDir {
		t.Errorf("stat /dir: fi=%+v err=%v", fi, err)
	}
}

func TestBuildFromInventoryGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(`"bkt","x","1","2016-11-30T00:00:00Z","e"` + "\n"))
	_ = gz.Close()
	data := buf.Bytes()

	open := func(_ context.Context, _ string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	ix, err := BuildFromInventory(context.Background(), InventoryOptions{
		Options:  Options{Bucket: "bkt"},
		Manifest: Manifest{FileFormat: "CSV", FileSchema: "Bucket, Key, Size, LastModifiedDate, ETag", Files: []ManifestFile{{Key: "f"}}},
		Open:     open,
		GzipData: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Stat("/x"); err != nil {
		t.Errorf("stat /x: %v", err)
	}
}

func TestBuildFromInventoryParquetUnsupported(t *testing.T) {
	_, err := BuildFromInventory(context.Background(), InventoryOptions{
		Manifest: Manifest{FileFormat: "Parquet", FileSchema: "Bucket, Key"},
		Open:     func(context.Context, string) (io.ReadCloser, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "Parquet") {
		t.Fatalf("expected a Parquet-unsupported error, got %v", err)
	}
}
