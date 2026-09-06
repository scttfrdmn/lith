// SPDX-License-Identifier: Apache-2.0

// Package s3client defines the narrow S3 surface lith depends on and provides
// a tuned aws-sdk-go-v2 implementation of it. The interface is deliberately
// small so tests can drive the index and read paths against an in-process
// fake (see the fake subpackage) with no network. See the pinned Design
// issue, §4.5.
package s3client

import (
	"context"
	"io"
	"time"
)

// Object is the subset of S3 object metadata lith records.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	// ETag is the raw ETag as returned by S3, including surrounding quotes.
	ETag string
}

// ListPage is one page of a ListObjectsV2 response.
type ListPage struct {
	Objects     []Object
	IsTruncated bool
	// NextToken is the continuation token for the next page, empty when done.
	NextToken string
}

// API is the S3 operations lith uses. Both the real client and the test fake
// implement it.
type API interface {
	// ListObjectsV2 lists keys under prefix without a delimiter. token is the
	// continuation token ("" for the first page); maxKeys caps the page size.
	ListObjectsV2(ctx context.Context, prefix, token string, maxKeys int32) (ListPage, error)
	// HeadObject returns metadata for a single key.
	HeadObject(ctx context.Context, key string) (Object, error)
	// GetObject returns a reader for [off, off+length) of key. A length <= 0
	// reads to the end of the object.
	GetObject(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
}
