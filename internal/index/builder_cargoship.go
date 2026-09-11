// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"fmt"
	"io"

	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/s3client"
)

// CargoshipOptions configures a CargoShip-backed index build.
type CargoshipOptions struct {
	Options
	// ManifestBytes is the raw (already-fetched, decompressed) manifest JSON.
	ManifestBytes []byte
	// ManifestSHA is the sha256 of the raw supplied manifest bytes (provenance).
	ManifestSHA [32]byte
}

// maxChainDepth bounds an incremental manifest chain walk (cycle/abuse guard).
const maxChainDepth = 256

// BuildFromCargoship builds an index from a resolved CargoShip archive by
// HEAD-ing each chunk for its ETag and folding provenance in. It is the
// low-level entry used by BuildFromCargoshipManifest and by tests that supply a
// prebuilt archive; ordinary callers use BuildFromCargoshipManifest.
func buildResolved(ctx context.Context, fetch s3client.API, arch *cargoship.Archive, sha [32]byte, uploadID, version, features string, opts Options) (*Index, error) {
	etags := make([]uint64, len(arch.Chunks))
	for i, c := range arch.Chunks {
		obj, err := fetch.HeadObject(ctx, c.Key)
		if err != nil {
			return nil, fmt.Errorf("cargoship: HEAD chunk %s: %w", c.Key, err)
		}
		etags[i] = HashETag(obj.ETag)
	}
	return BuildFromCargoship(arch, etags, sha, uploadID, version, features, opts)
}

// BuildFromCargoshipManifest parses a 2.1 manifest, walks any incremental chain
// (previous_manifest_id) to a union-of-latest tree, HEADs each chunk for its
// ETag, and builds the index. The chunk objects and any previous manifests are
// fetched from the same bucket, under the manifest's own prefix.
func BuildFromCargoshipManifest(ctx context.Context, fetch s3client.API, opts CargoshipOptions) (*Index, error) {
	m, err := cargoship.Parse(opts.ManifestBytes)
	if err != nil {
		return nil, err
	}
	arch, err := m.Resolve()
	if err != nil {
		return nil, err
	}
	feat := "frames"

	// Incremental chain: walk previous_manifest_id back, adding files whose path
	// is not already present (latest wins), and their chunks. Cycle-guarded.
	if m.PreviousManifestID != "" {
		seen := map[string]bool{m.UploadID: true}
		havePath := map[string]bool{}
		for _, f := range arch.Files {
			havePath[f.Path] = true
		}
		prevID := m.PreviousManifestID
		for depth := 0; prevID != "" && depth < maxChainDepth; depth++ {
			if seen[prevID] {
				break // cycle
			}
			seen[prevID] = true
			pm, perr := fetchPrevManifest(ctx, fetch, m.Prefix, prevID)
			if perr != nil {
				// A missing ancestor ends the chain — the union-of-latest is complete
				// with what we have (an ancestor may have expired).
				break
			}
			parch, rerr := pm.Resolve()
			if rerr != nil {
				return nil, fmt.Errorf("cargoship: previous manifest %s: %w", prevID, rerr)
			}
			base := len(arch.Chunks)
			arch.Chunks = append(arch.Chunks, parch.Chunks...)
			for _, f := range parch.Files {
				if havePath[f.Path] {
					continue
				}
				havePath[f.Path] = true
				for i := range f.Parts {
					f.Parts[i].ChunkIndex += base
				}
				arch.Files = append(arch.Files, f)
			}
			prevID = pm.PreviousManifestID
		}
	}

	return buildResolved(ctx, fetch, arch, opts.ManifestSHA, m.UploadID, m.Version, feat, opts.Options)
}

// fetchPrevManifest fetches and parses a previous manifest in the chain. Its key
// is <prefix>/uploads/<id>/manifest.json.gz (gz transparent).
func fetchPrevManifest(ctx context.Context, fetch s3client.API, prefix, id string) (*cargoship.Manifest, error) {
	key := prefix
	if key != "" {
		key += "/"
	}
	key += "uploads/" + id + "/manifest.json.gz"
	rc, err := fetch.GetObject(ctx, key, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	dec, err := MaybeGunzip(rc)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(dec, cargoship.MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	return cargoship.Parse(data)
}

// ReadManifestBytes reads a manifest source (an io.Reader), gunzips if needed,
// and returns the decompressed bytes bounded by the manifest cap. The caller
// supplies the raw sha256 separately for provenance.
func ReadManifestBytes(r io.Reader) ([]byte, error) {
	dec, err := MaybeGunzip(r)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(dec, cargoship.MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cargoship.MaxManifestBytes {
		return nil, fmt.Errorf("cargoship manifest exceeds the %d-byte cap", cargoship.MaxManifestBytes)
	}
	return data, nil
}
