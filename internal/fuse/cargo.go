// SPDX-License-Identifier: Apache-2.0

package fuse

// CargoShip virtual-file reads (#94). A cargo handle's bytes live in one or more
// packed `.tar.zst` chunks; a read maps to the covering part(s), each translated
// to its chunk's uncompressed tar-stream offset and served through the block
// store's frame-decode backing. Files were packed in tree order, so a
// directory-order walk reads each chunk sequentially — readahead runs on the
// chunk's uncompressed stream, turning many small objects into large-object
// streaming.
func (f *rawFS) readCargo(h *fileHandle, off, end int64) ([]byte, error) {
	out := make([]byte, 0, end-off)
	for _, p := range h.cargo {
		pStart, pEnd := p.fileOff, p.fileOff+p.length
		s, e := off, end
		if pStart > s {
			s = pStart
		}
		if pEnd < e {
			e = pEnd
		}
		if e <= s {
			continue
		}
		chunkOff := p.archiveOff + (s - p.fileOff)
		d, err := f.store.GetRange(f.ctx, p.key, chunkOff, e-s, p.uncompTotal)
		if err != nil {
			return nil, err
		}
		out = append(out, d...)
		f.cargoReadahead(h, p, chunkOff+(e-s))
	}
	return out, nil
}

// cargoReadahead drives the per-handle adaptive window on the chunk's
// uncompressed stream from the block this read reached.
func (f *rawFS) cargoReadahead(h *fileHandle, p cargoPart, chunkOff int64) {
	blk := chunkOff / f.blockSize
	for _, pb := range h.pf.observe(blk, f.perHandleWindow()) {
		pb := pb
		go f.store.Prefetch(f.ctx, p.key, pb, p.uncompTotal)
	}
}
