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
}

// checkStatus is the outcome of one diagnostic.
type checkStatus int

const (
	pass checkStatus = iota
	fail
	na
	info
)

func (s checkStatus) tag() string {
	switch s {
	case pass:
		return "PASS"
	case fail:
		return "FAIL"
	case na:
		return "N/A "
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
	if s == fail && fix != "" {
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

	// --- bucket existence + region + LIST access ---
	if target != "" && cfgErr == nil {
		s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.UsePathStyle = f.pathStyle
			if f.endpoint != "" {
				o.BaseEndpoint = aws.String(f.endpoint)
			}
		})
		doctorBucket(ctx, d, f, s3c, awsCfg, bucket)
		doctorList(ctx, d, s3c, bucket, prefix)
	}

	// --- FUSE preconditions ---
	doctorFUSE(d, f)

	// --- mountpoint ---
	if f.mountpoint != "" {
		doctorMountpoint(d, f.mountpoint)
	}

	// --- NIC facts ---
	nic := resolveNIC(ctx, os.TempDir(), f.nicGbps)
	inflight := int64(nic.BaselineGbps * 1e9 / 8 * 0.1 * 2) // 2 × NIC × 100ms (the mount default)
	d.add(info, "nic", fmt.Sprintf("%.1f Gbps (source=%s); inflight-bytes budget ≈ %d MiB",
		nic.BaselineGbps, nic.Source, inflight/(1<<20)), "")

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

func doctorBucket(ctx context.Context, d *doctor, f *doctorFlags, s3c *s3.Client, awsCfg aws.Config, bucket string) {
	// GetBucketRegion works signed or anonymous and returns the bucket's real
	// region (or a clear not-found/denied error) — the reliable existence+region
	// probe, unlike an anonymous HeadBucket which 403s on public buckets.
	bregion, err := manager.GetBucketRegion(ctx, s3c, bucket)
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || strings.Contains(apiErr.ErrorCode(), "NoSuchBucket")) {
			d.add(fail, "bucket", fmt.Sprintf("%q does not exist", bucket), "check the bucket name")
			return
		}
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "Forbidden" || apiErr.ErrorCode() == "AccessDenied") {
			d.add(fail, "bucket", fmt.Sprintf("access denied to %q", bucket), "check the credentials/policy, or --no-sign-request for a public bucket")
			return
		}
		d.add(fail, "bucket", fmt.Sprintf("cannot reach %q: %v", bucket, err), "check network/credentials")
		return
	}
	clientRegion := f.region
	if clientRegion == "" {
		clientRegion = "us-east-1"
	}
	if f.endpoint == "" && f.region != "" && bregion != "" && clientRegion != bregion {
		d.add(fail, "bucket region",
			fmt.Sprintf("bucket %q is in %s but the client is set to %s — cross-region reads are slow and cost egress", bucket, bregion, clientRegion),
			fmt.Sprintf("run the client in %s, or pass --region %s", bregion, bregion))
		return
	}
	d.add(pass, "bucket", fmt.Sprintf("%q reachable, region %s", bucket, bregion), "")
}

func doctorList(ctx context.Context, d *doctor, s3c *s3.Client, bucket, prefix string) {
	one := int32(1)
	_, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix, MaxKeys: &one})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "AccessDenied" || apiErr.ErrorCode() == "Forbidden") {
			d.add(na, "list", "ListObjectsV2 denied (not fatal)", "build the index from a key list: --keys <file> / --keys-from-manifest (Common Crawl, nyc-tlc)")
			return
		}
		d.add(fail, "list", "ListObjectsV2 failed: "+err.Error(), "check the prefix/credentials")
		return
	}
	d.add(pass, "list", "ListObjectsV2 ok", "")
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
		data, _, err := client.GetRange(ctx, curKey, 0, 0)
		if err != nil {
			d.add(fail, "CURRENT", "GET "+curKey+": "+err.Error(), "publish the dataset, or check the prefix")
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
