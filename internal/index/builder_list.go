// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// ListOptions configures building an index from ListObjectsV2.
type ListOptions struct {
	Options
	// Shards, if non-empty, are relative sub-prefixes listed concurrently and
	// merged; otherwise a single paginated listing runs under Prefix.
	Shards []string
	// PageSize is the ListObjectsV2 page size (defaults to 1000).
	PageSize int32
	// ProgressEvery logs progress after this many keys (defaults to 100000).
	ProgressEvery int
	// MaxKeys, if > 0, aborts the build with ErrTooManyKeys once more than
	// MaxKeys objects have been listed. Used by `mount` so it never silently
	// lists a huge bucket without an explicit `index build`.
	MaxKeys int
}

// ErrTooManyKeys is returned by BuildFromList when a build exceeds MaxKeys.
var ErrTooManyKeys = errors.New("index: bucket exceeds the auto-index limit; run `lith index build` explicitly")

// BuildFromList builds an index by listing the bucket with ListObjectsV2 (no
// delimiter), optionally sharded across sub-prefixes, then finalizing.
func BuildFromList(ctx context.Context, api s3client.API, opts ListOptions) (*Index, error) {
	root := normalizePrefix(opts.Prefix)
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	every := opts.ProgressEvery
	if every <= 0 {
		every = 100000
	}

	var (
		mu        sync.Mutex
		all       []Entry
		total     int
		lastLog   int
		overLimit bool
	)
	appendPage := func(objs []s3client.Object) {
		mu.Lock()
		defer mu.Unlock()
		for _, o := range objs {
			rel := strings.TrimPrefix(o.Key, root)
			all = append(all, Entry{
				Key:      rel,
				Size:     o.Size,
				MTime:    o.LastModified.UnixNano(),
				ETagHash: HashETag(o.ETag),
			})
		}
		total += len(objs)
		if opts.MaxKeys > 0 && total > opts.MaxKeys {
			overLimit = true
		}
		if opts.Logger != nil && total-lastLog >= every {
			lastLog = total
			opts.Logger.Info("listing progress", "keys", total)
		}
	}
	isOver := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return overLimit
	}

	listPrefix := func(ctx context.Context, prefix string) error {
		token := ""
		for {
			page, err := api.ListObjectsV2(ctx, prefix, token, pageSize)
			if err != nil {
				return err
			}
			appendPage(page.Objects)
			if isOver() {
				return ErrTooManyKeys
			}
			if !page.IsTruncated || page.NextToken == "" {
				return nil
			}
			token = page.NextToken
		}
	}

	start := time.Now()
	if len(opts.Shards) == 0 {
		if err := listPrefix(ctx, root); err != nil {
			return nil, err
		}
	} else {
		g, gctx := newGroup(ctx)
		for _, shard := range opts.Shards {
			prefix := root + strings.TrimPrefix(shard, "/")
			g.go_(func() error { return listPrefix(gctx, prefix) })
		}
		if err := g.wait(); err != nil {
			return nil, err
		}
	}

	if opts.Logger != nil {
		elapsed := time.Since(start).Seconds()
		rate := 0.0
		if elapsed > 0 {
			rate = float64(total) / elapsed
		}
		opts.Logger.Info("listing complete", "keys", total, "seconds", elapsed, "keys_per_sec", rate)
	}

	bopts := opts.Options
	bopts.Prefix = root
	bopts.Source = "list"
	return Build(all, bopts), nil
}

// small errgroup-like helper to avoid a dependency.
type group struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	err    error
	cancel context.CancelFunc
}

func newGroup(ctx context.Context) (*group, context.Context) {
	c, cancel := context.WithCancel(ctx)
	return &group{cancel: cancel}, c
}

func (g *group) go_(fn func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				g.cancel()
			}
			g.mu.Unlock()
		}
	}()
}

func (g *group) wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}
