// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/pointer"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/scttfrdmn/lith/internal/version"
	"github.com/spf13/cobra"
)

// doctorFlags mirror the S3 + FUSE flags that shape whether lith will actually
// work, so a doctor pass means the real mount will connect the same way.
type doctorFlags struct {
	region     string
	noSign     bool
	reqPays    bool
	endpoint   string
	pathStyle  bool
	indexFile  string
	mountpoint string
	allowOther bool
	nicGbps    float64
	// Alternate index sources. If any is set (or the target carries an @ref), a
	// LIST denial is not fatal — the build won't call ListObjectsV2.
	keys             string
	keysFromManifest string
	cargoship        string
}

// usesAltSource reports whether the invocation declares an index source other
// than a bucket LIST (so a LIST denial is genuinely not applicable).
func (f *doctorFlags) usesAltSource(ref string) bool {
	return ref != "" || f.keys != "" || f.keysFromManifest != "" || f.cargoship != ""
}

// checkStatus is the outcome of one diagnostic.
type checkStatus int

const (
	pass checkStatus = iota
	fail
	na
	info
	warn
)

func (s checkStatus) tag() string {
	switch s {
	case pass:
		return "PASS"
	case fail:
		return "FAIL"
	case na:
		return "N/A "
	case warn:
		return "WARN"
	default:
		return "INFO"
	}
}

// doctor accumulates results and whether any check failed.
type doctor struct {
	out    io.Writer
	failed bool
}

func (d *doctor) add(s checkStatus, name, detail, fix string) {
	if s == fail {
		d.failed = true
	}
	_, _ = fmt.Fprintf(d.out, "[%s] %-22s %s\n", s.tag(), name, detail)
	if (s == fail || s == warn) && fix != "" {
		_, _ = fmt.Fprintf(d.out, "        └─ fix: %s\n", fix)
	}
}

func newDoctorCmd() *cobra.Command {
	var f doctorFlags
	cmd := &cobra.Command{
		Use:   "doctor [s3://bucket[/prefix][@ref]]",
		Short: "Diagnose whether lith will work here — credentials, bucket access, FUSE, mountpoint",
		Long: `Run a series of environment and target checks, printing each as PASS, FAIL, or
N/A with a one-line fix for every failure. Exits non-zero if any check fails.

With no bucket argument, runs the environment checks only (credentials, FUSE, NIC).
A doctor pass is meant to mean the real mount connects the same way — every check
uses lith's own credential/endpoint resolution.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true, // check failures are the output; don't dump usage
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			d := &doctor{out: cmd.OutOrStdout()}
			runDoctor(cmd.Context(), d, &f, target)
			if d.failed {
				return fmt.Errorf("one or more checks failed")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.region, "region", "", "bucket region (resolved from the bucket if empty)")
	fl.BoolVar(&f.noSign, "no-sign-request", false, "anonymous requests (public buckets)")
	fl.BoolVar(&f.reqPays, "requester-pays", false, "add the requester-pays header")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint (S3-compatible stores)")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.indexFile, "index-file", "", "check this index file (exists, current format, root matches)")
	fl.StringVar(&f.mountpoint, "mountpoint", "", "check this mountpoint (exists, a dir, owned by you, empty)")
	fl.BoolVar(&f.allowOther, "allow-other", false, "also check /etc/fuse.conf user_allow_other")
	fl.Float64Var(&f.nicGbps, "nic-gbps", 0, "override the detected NIC bandwidth in Gbps")
	fl.StringVar(&f.keys, "keys", "", "index source: a key-list file (a LIST denial is then not fatal)")
	fl.StringVar(&f.keysFromManifest, "keys-from-manifest", "", "index source: keys from a manifest (a LIST denial is then not fatal)")
	fl.StringVar(&f.cargoship, "cargoship", "", "index source: a CargoShip manifest URL (a LIST denial is then not fatal)")
	return cmd
}

func runDoctor(ctx context.Context, d *doctor, f *doctorFlags, target string) {
	d.add(info, "lith", fmt.Sprintf("%s (commit %s, %s)", version.Version, version.Commit, version.Date), "")

	bucket, prefix, ref := "", "", ""
	if target != "" {
		var err error
		bucket, prefix, err = parseS3URL(target)
		if err != nil {
			d.add(fail, "target url", err.Error(), "use s3://bucket[/prefix][@current|@<version>]")
			target = ""
		} else {
			prefix, ref = splitPointerRef(prefix)
		}
	}

	// --- credentials (mirrors s3client.New's config load) ---
	awsCfg, cfgErr := doctorAWSConfig(ctx, f)
	switch {
	case cfgErr != nil:
		d.add(fail, "credentials", "load config: "+cfgErr.Error(), "check ~/.aws/config and the endpoint scheme (must be https unless --no-sign-request)")
	case f.noSign:
		d.add(info, "credentials", "anonymous (--no-sign-request)", "")
	default:
		creds, err := awsCfg.Credentials.Retrieve(ctx)
		if err != nil {
			d.add(fail, "credentials", "no credentials resolved: "+err.Error(), "set AWS_PROFILE (or export keys), or pass --no-sign-request for a public bucket")
		} else {
			d.add(pass, "credentials", "source="+credSource(creds), "")
		}
	}

	// --- bucket existence + region + LIST access, through the SAME factory the
	// mount uses, so doctor refuses exactly what mount would (e.g. a signed
	// request to an http:// endpoint) and the requester-pays header reaches the
	// probes (#194). A diagnostic that built its own client could pass on a
	// configuration the real client refuses. ---
	if target != "" && cfgErr == nil {
		doctorBucketAndList(ctx, d, f, awsCfg, bucket, prefix, ref)
	}

	// --- FUSE preconditions ---
	doctorFUSE(d, f)

	// --- mountpoint ---
	if f.mountpoint != "" {
		doctorMountpoint(d, f.mountpoint)
	}

	// --- NIC facts + the device-derived knobs mount will actually use (#237/#239) ---
	nic := resolveNIC(ctx, os.TempDir(), f.nicGbps)
	nicBytesPerSec := int64(nic.BaselineGbps * 1e9 / 8)
	inflight := int64(2 * float64(nicBytesPerSec) * 0.1) // 2 × NIC × 100ms (the mount default)
	partsMax := int64(float64(nicBytesPerSec) * 0.04)    // NIC × TTFB(40ms), clamped [4MiB,64MiB]
	if partsMax < 4<<20 {
		partsMax = 4 << 20
	} else if partsMax > 64<<20 {
		partsMax = 64 << 20
	}
	detail := fmt.Sprintf("%.1f Gbps (source=%s) → parts-max %d MiB, inflight %d MiB",
		nic.BaselineGbps, nic.Source, partsMax/(1<<20), inflight/(1<<20))
	switch nic.Source {
	case "fallback":
		// Every detection path failed; the values above are assumptions, not
		// measurements. WARN (not INFO) so an all-PASS report does not hide it.
		d.add(warn, "nic", detail+" — NIC undetected (no ethtool speed, no IMDS type, no DescribeInstanceTypes)",
			"pass --nic-gbps <Gbps>; without it the device-derived knobs are guesses (e.g. parts-max)")
	case "imds-estimate":
		d.add(info, "nic", detail+fmt.Sprintf(" — estimated from %s size (DescribeInstanceTypes denied); pass --nic-gbps for the exact baseline", nic.InstanceType), "")
	default:
		d.add(info, "nic", detail, "")
	}

	// Peak-vs-baseline guard (#239): --nic-gbps wants the *sustained baseline*, but
	// the number AWS advertises ("Up to N Gigabit") is the *peak*. Passing the peak
	// over-sizes the readahead window and fetches bytes that are never read for no
	// speed gain. When an override is set, re-detect the instance's true baseline
	// (ignoring the override) and warn if the override looks like the advertised
	// peak — substantially above the detected baseline.
	if f.nicGbps > 0 {
		det := resolveNIC(ctx, os.TempDir(), 0)
		switch {
		case det.PeakGbps > det.BaselineGbps && f.nicGbps >= det.PeakGbps*0.95:
			d.add(warn, "nic-gbps",
				fmt.Sprintf("--nic-gbps %.1f matches this instance's peak (baseline %.1f, peak %.1f, source=%s)",
					f.nicGbps, det.BaselineGbps, det.PeakGbps, det.Source),
				fmt.Sprintf("--nic-gbps wants the sustained baseline (%.1f), not the 'Up to N Gigabit' peak — the peak over-fetches for no speed gain", det.BaselineGbps))
		case det.Source != "fallback" && det.BaselineGbps > 0 && f.nicGbps >= det.BaselineGbps*1.5:
			d.add(warn, "nic-gbps",
				fmt.Sprintf("--nic-gbps %.1f is well above the detected baseline %.1f (source=%s)", f.nicGbps, det.BaselineGbps, det.Source),
				"--nic-gbps wants the sustained baseline, not the advertised 'Up to N Gigabit' peak; overstating it over-fetches for no speed gain")
		}
	}

	// --- pointer / index target ---
	if target != "" && cfgErr == nil {
		if ref != "" {
			doctorPointer(ctx, d, f, bucket, prefix, ref)
		}
	}
	if f.indexFile != "" {
		doctorIndexFile(d, f.indexFile, prefix)
	}
}

// doctorAWSConfig builds the aws.Config the same way s3client.New does, so the
// doctor's checks use lith's real credential/endpoint resolution.
func doctorAWSConfig(ctx context.Context, f *doctorFlags) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if f.noSign {
		opts = append(opts, awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}
	region := f.region
	if region == "" {
		region = "us-east-1" // bootstrap region for resolution
	}
	opts = append(opts, awsconfig.WithRegion(region))
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// doctorBucketAndList builds the client through the production factory
// (s3client.New), so its refusals — the non-HTTPS-endpoint guard above all —
// become check failures with the factory's own message, and the requester-pays
// header reaches the probes. New succeeding already proves the bucket is
// reachable and resolves its region.
func doctorBucketAndList(ctx context.Context, d *doctor, f *doctorFlags, awsCfg aws.Config, bucket, prefix, ref string) {
	client, err := newS3Client(ctx, s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSign,
		RequesterPays: f.reqPays, Endpoint: f.endpoint, PathStyle: f.pathStyle,
	})
	if err != nil {
		classifyBucketClientError(d, bucket, err)
		return
	}

	// Region-mismatch diagnostic: cross-region reads are slow and cost egress.
	// Only meaningful against real S3 (a custom --endpoint has no AWS region to
	// compare), and the endpoint already passed the factory's HTTPS guard above.
	regionNote := ""
	if f.endpoint == "" {
		if bregion, rerr := manager.GetBucketRegion(ctx, s3.NewFromConfig(awsCfg), bucket); rerr == nil && bregion != "" {
			if f.region != "" && f.region != bregion {
				d.add(fail, "bucket region",
					fmt.Sprintf("bucket %q is in %s but the client is set to %s — cross-region reads are slow and cost egress", bucket, bregion, f.region),
					fmt.Sprintf("run the client in %s, or pass --region %s", bregion, bregion))
				return
			}
			regionNote = ", region " + bregion
		}
	}
	d.add(pass, "bucket", fmt.Sprintf("%q reachable%s", bucket, regionNote), "")
	doctorList(ctx, d, client, prefix, f.usesAltSource(ref))
}

// classifyBucketClientError reports a factory failure with its own message. The
// HTTPS-endpoint guard is reported as an endpoint failure (the case the mount
// path refuses); not-found / access-denied / unreachable map to a bucket
// failure, classified by the wrapped error string.
func classifyBucketClientError(d *doctor, bucket string, err error) {
	es := err.Error()
	switch {
	case strings.Contains(es, "non-HTTPS"):
		d.add(fail, "endpoint", es, "use an https:// endpoint, or pass --no-sign-request for anonymous access")
	case strings.Contains(es, "NoSuchBucket") || strings.Contains(es, "bucket not found") || strings.Contains(es, "status code: 404"):
		d.add(fail, "bucket", fmt.Sprintf("%q does not exist", bucket), "check the bucket name")
	case strings.Contains(es, "AccessDenied") || strings.Contains(es, "Forbidden") || strings.Contains(es, "status code: 403"):
		d.add(fail, "bucket", fmt.Sprintf("access denied to %q", bucket), "check the credentials/policy, or --no-sign-request for a public bucket")
	default:
		d.add(fail, "bucket", fmt.Sprintf("cannot reach %q: %v", bucket, err), "check network/credentials/endpoint")
	}
}

// doctorList probes a one-key LIST through the production client (so
// requester-pays applies). A denial is a FAILURE — the mount's default build
// path needs ListBucket — unless the invocation declares an alternate index
// source (@ref / --keys / --keys-from-manifest / --cargoship), in which case
// LIST is genuinely not applicable.
func doctorList(ctx context.Context, d *doctor, client s3client.API, prefix string, altSource bool) {
	if _, err := client.ListObjectsV2(ctx, prefix, "", 1); err != nil {
		if isAccessDenied(err) {
			if altSource {
				d.add(na, "list", "ListObjectsV2 denied — using an explicit key source instead", "")
				return
			}
			d.add(fail, "list", "ListObjectsV2 denied", "grant s3:ListBucket, or build from an explicit source: --keys / --keys-from-manifest / --cargoship (then LIST is not needed)")
			return
		}
		d.add(fail, "list", "ListObjectsV2 failed: "+err.Error(), "check the prefix/credentials")
		return
	}
	d.add(pass, "list", "ListObjectsV2 ok", "")
}

// isAccessDenied reports whether err is an S3 access-denial, by smithy code or
// by the wrapped error string (the client wraps the SDK error).
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "AccessDenied" || apiErr.ErrorCode() == "Forbidden") {
		return true
	}
	es := err.Error()
	return strings.Contains(es, "AccessDenied") || strings.Contains(es, "Forbidden") || strings.Contains(es, "status code: 403")
}

func doctorFUSE(d *doctor, f *doctorFlags) {
	if runtime.GOOS != "linux" {
		d.add(na, "fuse", runtime.GOOS+" is not a lith mount target (Linux only)", "")
		return
	}
	if fh, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		if os.IsNotExist(err) {
			d.add(fail, "fuse device", "/dev/fuse is missing", "install fuse3 (apt install fuse3) or load the fuse kernel module")
		} else {
			d.add(fail, "fuse device", "/dev/fuse not openable: "+err.Error(), "add your user to a group with /dev/fuse access, or run via a container with --device /dev/fuse")
		}
	} else {
		_ = fh.Close()
		d.add(pass, "fuse device", "/dev/fuse present and openable", "")
	}
	if p, err := exec.LookPath("fusermount3"); err != nil {
		d.add(fail, "fusermount3", "not on PATH", "install fuse3 (apt install fuse3)")
	} else {
		d.add(pass, "fusermount3", p, "")
	}
	if f.allowOther {
		b, err := os.ReadFile("/etc/fuse.conf")
		if err != nil || !hasUncommented(string(b), "user_allow_other") {
			d.add(fail, "user_allow_other", "not enabled in /etc/fuse.conf", "add a line 'user_allow_other' to /etc/fuse.conf (required for --allow-other)")
		} else {
			d.add(pass, "user_allow_other", "enabled in /etc/fuse.conf", "")
		}
	}
}

func doctorMountpoint(d *doctor, mp string) {
	fi, err := os.Lstat(mp)
	if err != nil {
		d.add(fail, "mountpoint", mp+": "+err.Error(), "mkdir -p "+mp+" (as your own user, not sudo)")
		return
	}
	if !fi.IsDir() {
		d.add(fail, "mountpoint", mp+" is not a directory", "point --mountpoint at a directory")
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		d.add(fail, "mountpoint", fmt.Sprintf("%s is owned by uid %d, not you (%d)", mp, st.Uid, os.Geteuid()),
			"a sudo-created mountpoint is root-owned and fusermount3 refuses it — chown it to your user, or mkdir it yourself")
		return
	}
	ents, err := os.ReadDir(mp)
	if err != nil {
		d.add(fail, "mountpoint", "cannot read "+mp+": "+err.Error(), "check permissions")
		return
	}
	if len(ents) > 0 {
		d.add(fail, "mountpoint", mp+" is not empty", "mount over an empty directory")
		return
	}
	d.add(pass, "mountpoint", mp+" (dir, yours, empty)", "")
}

func doctorPointer(ctx context.Context, d *doctor, f *doctorFlags, bucket, dataset, ref string) {
	client, err := newS3Client(ctx, s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSign,
		RequesterPays: f.reqPays, Endpoint: f.endpoint, PathStyle: f.pathStyle,
	})
	if err != nil {
		d.add(fail, "pointer", "s3 client: "+err.Error(), "")
		return
	}
	dataset = strings.Trim(dataset, "/")
	var indexKey, wantSHA, manifestKey string
	if ref == "current" {
		curKey := path.Join(dataset, "CURRENT")
		data, err := readPointerObject(ctx, client, curKey)
		if err != nil {
			d.add(fail, "CURRENT", err.Error(), "publish the dataset, or check the prefix")
			return
		}
		cur, err := pointer.Parse(data)
		if err != nil {
			d.add(fail, "CURRENT", curKey+": "+err.Error(), "the CURRENT pointer is malformed")
			return
		}
		d.add(pass, "CURRENT", "resolves → version "+cur.VersionID, "")
		indexKey, wantSHA, manifestKey = cur.IndexKey, cur.IndexSHA256, cur.ManifestKey
	} else {
		indexKey = path.Join(dataset, "v", ref, "index.lith")
		d.add(info, "pointer", "pinned version "+ref, "")
	}
	// index exists + sha
	img, _, err := client.GetRange(ctx, indexKey, 0, 0)
	if err != nil {
		d.add(fail, "index object", "GET "+indexKey+": "+err.Error(), "CURRENT names an index that does not exist")
		return
	}
	if wantSHA != "" {
		got := sha256.Sum256(img)
		if hex.EncodeToString(got[:]) != wantSHA {
			d.add(fail, "index sha256", indexKey+" does not match CURRENT", "the index changed under the pointer; republish")
			return
		}
		d.add(pass, "index sha256", "matches CURRENT", "")
	}
	ix, err := index.Unmarshal(img)
	if err != nil {
		d.add(fail, "index", indexKey+": "+err.Error(), "the index object is corrupt or an unsupported format")
		return
	}
	if ix.Len() == 0 {
		d.add(fail, "index", indexKey+" is empty (chunkless manifest?)", "publish from a framed archive, not a direct upload")
		return
	}
	d.add(pass, "index", fmt.Sprintf("%d files", ix.Len()), "")
	// manifest exists + has chunks
	if manifestKey != "" {
		mb, _, err := client.GetRange(ctx, manifestKey, 0, 0)
		if err != nil {
			d.add(fail, "manifest", "GET "+manifestKey+": "+err.Error(), "CURRENT names a manifest that does not exist")
			return
		}
		raw, rerr := index.ReadManifestBytes(strings.NewReader(string(mb)))
		if rerr != nil {
			d.add(fail, "manifest", manifestKey+": "+rerr.Error(), "the manifest is unreadable")
			return
		}
		if m, perr := cargoship.Parse(raw); perr != nil {
			d.add(fail, "manifest", manifestKey+": "+perr.Error(), "the manifest is not a valid CargoShip 2.1 manifest")
		} else if len(m.Chunks) == 0 {
			d.add(fail, "manifest", manifestKey+" has no chunks", "a chunkless (direct-upload) manifest is not mountable")
		} else {
			d.add(pass, "manifest", fmt.Sprintf("%d chunks", len(m.Chunks)), "")
		}
	}
}

func doctorIndexFile(d *doctor, path, prefix string) {
	if _, err := os.Stat(path); err != nil {
		d.add(fail, "index file", path+": "+err.Error(), "run 'lith index build' first")
		return
	}
	ix, closeIx, err := index.Open(path)
	if err != nil {
		d.add(fail, "index file", path+": "+err.Error(), "rebuild it: 'lith index build' (older-format indexes are not loadable)")
		return
	}
	defer func() { _ = closeIx() }()
	if _, err := ix.Root(prefix); err != nil {
		d.add(fail, "index root", fmt.Sprintf("%s does not cover prefix %q: %v", path, prefix, err), "build the index at or above the mount prefix")
		return
	}
	d.add(pass, "index file", fmt.Sprintf("%s (format v%d, %d files)", path, index.FormatVersion, ix.Len()), "")
}

// hasUncommented reports whether s has a non-comment line equal to token.
func hasUncommented(s, token string) bool {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == token {
			return true
		}
	}
	return false
}

// credSource maps an aws.Credentials to a short source label.
func credSource(c aws.Credentials) string {
	if c.Source == "" {
		return "unknown"
	}
	return c.Source
}
