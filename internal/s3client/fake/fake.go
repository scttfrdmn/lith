// SPDX-License-Identifier: Apache-2.0

// Package fake is an in-process implementation of the s3client.API surface
// (ListObjectsV2, HeadObject, GetObject with Range) over an in-memory map.
// It is used by lith's unit tests so that no test touches the network. See
// the pinned Design issue, §4 and issue #15.
package fake

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// object is a stored object: its bytes and metadata.
type object struct {
	data     []byte
	modified time.Time
	etag     string
}

// Server is an in-memory fake S3 bucket implementing s3client.API.
type Server struct {
	mu   sync.RWMutex
	objs map[string]object
	// DefaultPageSize caps ListObjectsV2 page size when the caller passes a
	// larger or non-positive maxKeys (mirrors S3's 1000 cap).
	DefaultPageSize int32
	// ListCalls counts ListObjectsV2 invocations, useful for asserting the
	// "zero S3 calls after index build" property in tests.
	ListCalls int
	GetCalls  int
	HeadCalls int
}

// New returns an empty fake server.
func New() *Server {
	return &Server{objs: make(map[string]object), DefaultPageSize: 1000}
}

// Put stores an object with the given key, contents, and modification time.
func (s *Server) Put(key string, data []byte, modified time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := md5.Sum(data)
	s.objs[key] = object{
		data:     append([]byte(nil), data...),
		modified: modified,
		etag:     fmt.Sprintf("%q", fmt.Sprintf("%x", sum)),
	}
}

// PutString is a convenience wrapper around Put for string contents.
func (s *Server) PutString(key, data string, modified time.Time) {
	s.Put(key, []byte(data), modified)
}

var _ s3client.API = (*Server)(nil)

// ListObjectsV2 returns keys >= the continuation token that share prefix,
// in sorted order, one page at a time.
func (s *Server) ListObjectsV2(_ context.Context, prefix, token string, maxKeys int32) (s3client.ListPage, error) {
	s.mu.Lock()
	s.ListCalls++
	s.mu.Unlock()

	s.mu.RLock()
	keys := make([]string, 0, len(s.objs))
	for k := range s.objs {
		if prefix == "" || hasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	s.mu.RUnlock()
	sort.Strings(keys)

	page := s.DefaultPageSize
	if maxKeys > 0 && maxKeys < page {
		page = maxKeys
	}
	if page <= 0 {
		page = 1000
	}

	var out s3client.ListPage
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range keys {
		if token != "" && k <= token {
			continue
		}
		if int32(len(out.Objects)) == page {
			out.IsTruncated = true
			out.NextToken = out.Objects[len(out.Objects)-1].Key
			break
		}
		o := s.objs[k]
		out.Objects = append(out.Objects, s3client.Object{
			Key:          k,
			Size:         int64(len(o.data)),
			LastModified: o.modified,
			ETag:         o.etag,
		})
	}
	return out, nil
}

// HeadObject returns metadata for key.
func (s *Server) HeadObject(_ context.Context, key string) (s3client.Object, error) {
	s.mu.Lock()
	s.HeadCalls++
	s.mu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objs[key]
	if !ok {
		return s3client.Object{}, &NotFoundError{Key: key}
	}
	return s3client.Object{
		Key:          key,
		Size:         int64(len(o.data)),
		LastModified: o.modified,
		ETag:         o.etag,
	}, nil
}

// GetObject returns [off, off+length) of key. length <= 0 reads to the end.
func (s *Server) GetObject(_ context.Context, key string, off, length int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.GetCalls++
	s.mu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objs[key]
	if !ok {
		return nil, &NotFoundError{Key: key}
	}
	if off < 0 {
		off = 0
	}
	if off > int64(len(o.data)) {
		off = int64(len(o.data))
	}
	end := int64(len(o.data))
	if length > 0 && off+length < end {
		end = off + length
	}
	return io.NopCloser(bytes.NewReader(o.data[off:end])), nil
}

// NotFoundError is returned for a missing key.
type NotFoundError struct{ Key string }

func (e *NotFoundError) Error() string { return "fake s3: key not found: " + e.Key }

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
