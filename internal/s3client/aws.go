// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Config controls construction of the AWS-backed client.
type Config struct {
	Bucket string
	// Region, if empty, is resolved from the bucket at construction time.
	Region string
	// NoSignRequest sends anonymous (unsigned) requests, for public buckets.
	NoSignRequest bool
	// RequesterPays adds the requester-pays header to every request.
	RequesterPays bool
	// Endpoint overrides the S3 endpoint (for S3-compatible stores).
	Endpoint string
	// PathStyle forces path-style addressing (bucket in the path).
	PathStyle bool
	// Concurrency sizes the connection pool; defaults to 64.
	Concurrency int
	// TransportWrap, when non-nil, wraps the tuned RoundTripper before use.
	// Used only by diagnostic tooling (e.g. lith-s3bench / bench instrumentation)
	// to count HTTP attempts by status code; nil in production.
	TransportWrap func(http.RoundTripper) http.RoundTripper
}

// Client is the aws-sdk-go-v2 implementation of API with a transport tuned
// for S3 (see design §4.5).
type Client struct {
	s3            *s3.Client
	bucket        string
	requesterPays bool
}

var _ API = (*Client)(nil)

// TuneTransport configures t for many concurrent range GETs against S3: a
// large connection pool, keep-alives, and HTTP/1.1 (S3 does not benefit from
// HTTP/2). Fields are set individually to avoid copying the Transport's lock.
// Exported so diagnostic tooling (cmd/lith-s3bench) exercises the exact same
// transport lith uses, rather than a divergent copy.
func TuneTransport(t *http.Transport, concurrency int) {
	if concurrency <= 0 {
		concurrency = 64
	}
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.ForceAttemptHTTP2 = false
	t.MaxConnsPerHost = 0 // unlimited; bounded by the caller's worker pool
	t.MaxIdleConns = concurrency * 2
	t.MaxIdleConnsPerHost = concurrency
	t.IdleConnTimeout = 90 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ExpectContinueTimeout = time.Second
}

// TransportInfo describes the tuned transport for a given concurrency, for
// diagnostics/logging. It mirrors what TuneTransport sets.
func TransportInfo(concurrency int) string {
	if concurrency <= 0 {
		concurrency = 64
	}
	return fmt.Sprintf("http2=off MaxConnsPerHost=0(unlimited) MaxIdleConnsPerHost=%d IdleConnTimeout=90s", concurrency)
}

// buildTransport returns a freshly tuned transport (used by tests).
func buildTransport(concurrency int) *http.Transport {
	t := &http.Transport{}
	TuneTransport(t, concurrency)
	return t
}

// New constructs a Client. When cfg.Region is empty and no endpoint override
// is set, the region is resolved from the bucket. This performs network I/O;
// unit tests use the fake instead.
func New(ctx context.Context, cfg Config) (*Client, error) {
	// M3: when signing is enabled, refuse to send SigV4-signed requests (which
	// carry credentials, including any X-Amz-Security-Token) to a non-HTTPS
	// endpoint, where they could be observed or misdirected in cleartext. This
	// must run before any signed request is made (region resolution below).
	if cfg.Endpoint != "" && !cfg.NoSignRequest {
		if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme != "https" {
			return nil, fmt.Errorf("s3client: --endpoint %q uses a non-HTTPS scheme; signed requests may not be sent in cleartext — use https, or pass --no-sign-request for anonymous access", cfg.Endpoint)
		}
	}

	var httpClient config.HTTPClient
	if cfg.TransportWrap != nil {
		// Diagnostic path: build the tuned transport, wrap its RoundTripper, and
		// hand it over as a plain *http.Client so every HTTP attempt is observed.
		t := &http.Transport{}
		TuneTransport(t, cfg.Concurrency)
		httpClient = &http.Client{Transport: cfg.TransportWrap(t)}
	} else {
		httpClient = awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
			TuneTransport(t, cfg.Concurrency)
		})
	}

	loadOpts := []func(*config.LoadOptions) error{
		config.WithHTTPClient(httpClient),
	}
	if cfg.NoSignRequest {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1" // bootstrap region for resolution
	}
	loadOpts = append(loadOpts, config.WithRegion(region))

	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3client: load config: %w", err)
	}

	// M3 (continued): the flag-only pre-check above catches --endpoint, but the
	// SDK also resolves AWS_ENDPOINT_URL / AWS_ENDPOINT_URL_S3 / shared-config
	// endpoint_url into awsCfg.BaseEndpoint. Validate the *effective* endpoint —
	// the one s3.NewFromConfig will actually use — so a cleartext endpoint from
	// the environment or shared config cannot reopen the credential-leak hole.
	if !cfg.NoSignRequest {
		effective, source := cfg.Endpoint, "--endpoint"
		if effective == "" && awsCfg.BaseEndpoint != nil {
			effective, source = *awsCfg.BaseEndpoint, "the environment or shared config (AWS_ENDPOINT_URL / endpoint_url)"
		}
		if effective != "" {
			if u, err := url.Parse(effective); err != nil || u.Scheme != "https" {
				return nil, fmt.Errorf("s3client: endpoint %q (from %s) uses a non-HTTPS scheme; signed requests may not be sent in cleartext — use https, or pass --no-sign-request for anonymous access", effective, source)
			}
		}
	}

	s3Opts := func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}

	// Resolve the bucket region unless one was given or an endpoint overrides it.
	if cfg.Region == "" && cfg.Endpoint == "" {
		bootstrap := s3.NewFromConfig(awsCfg, s3Opts)
		resolved, err := manager.GetBucketRegion(ctx, bootstrap, cfg.Bucket)
		if err != nil {
			return nil, fmt.Errorf("s3client: resolve region for bucket %q: %w", cfg.Bucket, err)
		}
		if resolved != "" {
			awsCfg.Region = resolved
		}
	}

	return &Client{
		s3:            s3.NewFromConfig(awsCfg, s3Opts),
		bucket:        cfg.Bucket,
		requesterPays: cfg.RequesterPays,
	}, nil
}

func (c *Client) payer() types.RequestPayer {
	if c.requesterPays {
		return types.RequestPayerRequester
	}
	return ""
}

// ListObjectsV2 lists keys under prefix without a delimiter.
func (c *Client) ListObjectsV2(ctx context.Context, prefix, token string, maxKeys int32) (ListPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket:       &c.bucket,
		Prefix:       ptrOrNil(prefix),
		RequestPayer: c.payer(),
	}
	if token != "" {
		in.ContinuationToken = &token
	}
	if maxKeys > 0 {
		in.MaxKeys = aws.Int32(maxKeys)
	}
	out, err := c.s3.ListObjectsV2(ctx, in)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{IsTruncated: aws.ToBool(out.IsTruncated)}
	page.Objects = make([]Object, 0, len(out.Contents))
	for _, o := range out.Contents {
		page.Objects = append(page.Objects, Object{
			Key:          aws.ToString(o.Key),
			Size:         aws.ToInt64(o.Size),
			LastModified: aws.ToTime(o.LastModified),
			ETag:         aws.ToString(o.ETag),
		})
	}
	if page.IsTruncated {
		page.NextToken = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

// HeadObject returns metadata for key.
func (c *Client) HeadObject(ctx context.Context, key string) (Object, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       &c.bucket,
		Key:          &key,
		RequestPayer: c.payer(),
	})
	if err != nil {
		return Object{}, err
	}
	return Object{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		ETag:         aws.ToString(out.ETag),
	}, nil
}

// GetObject returns [off, off+length) of key. length <= 0 reads to the end.
func (c *Client) GetObject(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	var rng *string
	if length > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	} else if off > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-", off))
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       &c.bucket,
		Key:          &key,
		Range:        rng,
		RequestPayer: c.payer(),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// GetRange reads [off, off+length) of key into memory and returns the bytes
// and the object's ETag.
func (c *Client) GetRange(ctx context.Context, key string, off, length int64) ([]byte, string, error) {
	var rng *string
	if length > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	} else if off > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-", off))
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       &c.bucket,
		Key:          &key,
		Range:        rng,
		RequestPayer: c.payer(),
	})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = out.Body.Close() }()
	if rng != nil {
		if err := validateContentRange(aws.ToString(out.ContentRange), off); err != nil {
			return nil, "", err
		}
	}
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return data, aws.ToString(out.ETag), nil
}

// GetRangeReader returns a streaming reader for [off, off+length) of key and
// the object's ETag.
func (c *Client) GetRangeReader(ctx context.Context, key string, off, length int64) (io.ReadCloser, string, error) {
	var rng *string
	if length > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	} else if off > 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-", off))
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       &c.bucket,
		Key:          &key,
		Range:        rng,
		RequestPayer: c.payer(),
	})
	if err != nil {
		return nil, "", err
	}
	if rng != nil {
		if err := validateContentRange(aws.ToString(out.ContentRange), off); err != nil {
			_ = out.Body.Close()
			return nil, "", err
		}
	}
	return out.Body, aws.ToString(out.ETag), nil
}

// validateContentRange verifies that a ranged GetObject response actually
// returned partial content beginning at the requested offset. cr is the raw
// Content-Range header ("bytes <start>-<end>/<total>", per RFC 7233). A
// non-conforming or hostile endpoint that answers a ranged request with the
// whole object (HTTP 200, no/!matching Content-Range) would otherwise let the
// first length bytes be served as bytes [off, off+length); reject that. An
// empty, malformed, or mismatched-start value is an error.
func validateContentRange(cr string, off int64) error {
	rangeErr := func() error {
		return fmt.Errorf("s3client: endpoint did not honor the requested byte range (got %q, want start %d)", cr, off)
	}
	const prefix = "bytes "
	if !strings.HasPrefix(cr, prefix) {
		return rangeErr()
	}
	spec := cr[len(prefix):]
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return rangeErr()
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || start != off {
		return rangeErr()
	}
	return nil
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
