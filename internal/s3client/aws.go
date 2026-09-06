// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
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
}

// Client is the aws-sdk-go-v2 implementation of API with a transport tuned
// for S3 (see design §4.5).
type Client struct {
	s3            *s3.Client
	bucket        string
	requesterPays bool
}

var _ API = (*Client)(nil)

// tuneTransport configures t for many concurrent range GETs against S3: a
// large connection pool, keep-alives, and HTTP/1.1 (S3 does not benefit from
// HTTP/2). Fields are set individually to avoid copying the Transport's lock.
func tuneTransport(t *http.Transport, concurrency int) {
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

// buildTransport returns a freshly tuned transport (used by tests).
func buildTransport(concurrency int) *http.Transport {
	t := &http.Transport{}
	tuneTransport(t, concurrency)
	return t
}

// New constructs a Client. When cfg.Region is empty and no endpoint override
// is set, the region is resolved from the bucket. This performs network I/O;
// unit tests use the fake instead.
func New(ctx context.Context, cfg Config) (*Client, error) {
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		tuneTransport(t, cfg.Concurrency)
	})

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
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return data, aws.ToString(out.ETag), nil
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
