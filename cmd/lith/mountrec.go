// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// mountRecord is the small per-mount record lith writes so `lith mounts` and
// `lith umount` can find a mount by its mountpoint without scraping argv (#91).
type mountRecord struct {
	PID        int       `json:"pid"`
	Mountpoint string    `json:"mountpoint"`
	Bucket     string    `json:"bucket"`
	Root       string    `json:"root"`
	IndexFile  string    `json:"index_file"`
	Manifest   string    `json:"manifest,omitempty"` // cargoship manifest key for a --cargoship mount
	Start      time.Time `json:"start"`
	path       string    // the record file (not serialized)
}

// lithRunDir returns the per-user runtime directory for mount records:
// $XDG_RUNTIME_DIR/lith, else /run/user/<uid>/lith, else /tmp/lith-<uid>.
func lithRunDir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "lith")
	}
	uid := os.Getuid()
	if uid >= 0 {
		if d := fmt.Sprintf("/run/user/%d", uid); dirWritable(d) {
			return filepath.Join(d, "lith")
		}
		return filepath.Join(os.TempDir(), fmt.Sprintf("lith-%d", uid))
	}
	return filepath.Join(os.TempDir(), "lith")
}

// dirWritable reports whether an existing dir is a real directory (not a
// symlink) owned by the effective uid — the trust bar for using it to hold
// mount records. A missing dir is not "writable" here (callers create it 0700).
func dirWritable(d string) bool {
	fi, err := os.Lstat(d)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		return false
	}
	return true
}

// dirTrusted verifies that a dir is safe to use for mount records: if it exists
// it must be a directory (not a symlink) owned by the effective uid. A path that
// does not yet exist is trusted (it will be created 0700). This blocks an
// attacker who pre-creates the (predictable) run dir to plant forged records
// (finding H2/M2).
func dirTrusted(d string) error {
	fi, err := os.Lstat(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", d)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", d)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot verify ownership", d)
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is not owned by uid %d", d, os.Geteuid())
	}
	return nil
}

// recordPath is the record file for a mountpoint: <rundir>/<sanitized-abs>.json.
func recordPath(mountpoint string) string {
	abs, err := filepath.Abs(mountpoint)
	if err != nil {
		abs = mountpoint
	}
	id := strings.Trim(abs, "/")
	id = strings.ReplaceAll(id, "/", "-")
	if id == "" {
		id = "root"
	}
	return filepath.Join(lithRunDir(), id+".json")
}

// writeMountRecord persists a mount record (best-effort; a failure to record
// does not fail the mount). Returns the path written.
func writeMountRecord(rec mountRecord) (string, error) {
	dir := lithRunDir()
	if err := dirTrusted(dir); err != nil {
		return "", err
	}
	// Create the run dir with Mkdir (single level; the parent — $XDG_RUNTIME_DIR,
	// /run/user/<uid>, or /tmp — already exists) rather than MkdirAll: Mkdir does
	// not silently accept a pre-existing symlink an attacker swapped in after the
	// dirTrusted Lstat above (the /tmp/lith-<uid> fallback is the racy path).
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	// Re-verify AFTER creation and immediately before the write: closes the TOCTOU
	// window between the first dirTrusted and the create.
	if err := dirTrusted(dir); err != nil {
		return "", err
	}
	rec.path = recordPath(rec.Mountpoint)
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	// O_NOFOLLOW so a symlink planted at the record path is not followed;
	// O_TRUNC because a record is rewritten on each mount. os.WriteFile would
	// follow a symlink.
	f, err := os.OpenFile(rec.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return "", err
	}
	return rec.path, f.Close()
}

func removeMountRecord(mountpoint string) { _ = os.Remove(recordPath(mountpoint)) }

// listMountRecords reads all mount records in the run dir.
func listMountRecords() []mountRecord {
	dir := lithRunDir()
	// Never trust records from a run dir we do not own (finding H2): a pid read
	// from a forged record must never be signalled.
	if err := dirTrusted(dir); err != nil {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []mountRecord
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r mountRecord
		if json.Unmarshal(b, &r) == nil && r.Mountpoint != "" {
			r.path = p
			out = append(out, r)
		}
	}
	return out
}

// procMount is one /proc/mounts entry (source target fstype …).
type procMount struct{ Source, Target, Fstype string }

// parseProcMounts parses /proc/mounts content.
func parseProcMounts(r io.Reader) []procMount {
	b, _ := io.ReadAll(r)
	var out []procMount
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			out = append(out, procMount{Source: unescapeMount(f[0]), Target: unescapeMount(f[1]), Fstype: f[2]})
		}
	}
	return out
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces etc.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// procMountsReader is overridable in tests; production reads /proc/mounts.
var procMountsReader = func() []procMount {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return parseProcMounts(f)
}

// isLithMount reports whether a /proc/mounts entry is a lith FUSE mount. lith
// sets the FUSE subtype to "lith", so the fstype is "fuse.lith".
func isLithMount(pm procMount) bool {
	return pm.Fstype == "fuse.lith"
}

// mountedAt reports whether a lith FUSE mount is live at mountpoint.
func mountedAt(mountpoint string) bool {
	abs, err := filepath.Abs(mountpoint)
	if err != nil {
		abs = mountpoint
	}
	for _, pm := range procMountsReader() {
		if pm.Target == abs && isLithMount(pm) {
			return true
		}
	}
	return false
}
