// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// KeysOptions configures BuildFromKeys.
type KeysOptions struct {
	Options
	// Keys are the parsed key lines; each Key is relative to Options.Prefix.
	Keys []KeyEntry
	// Concurrency bounds the HeadObject fan-out for keys lacking size+mtime
	// (default 32).
	Concurrency int
	// AllowMissing, when true, skips (and counts) a key that HeadObject reports
	// as 403/404 instead of failing the build.
	AllowMissing bool
	// KeysSHA is the sha256 of the raw key file/manifest bytes (provenance).
	KeysSHA [32]byte
}

// KeysResult reports the outcome of a key-list build.
type KeysResult struct {
	Headed      int      // keys resolved via HeadObject
	Missing     int      // keys that HeadObject reported as 403/404
	MissingKeys []string // the relative keys that were missing
}

// httpStatuser is satisfied by the aws-sdk / smithy transport error types
// (both expose HTTPStatusCode); the test fake's NotFoundError implements it
// too. It lets BuildFromKeys classify a HeadObject failure without importing
// the AWS SDK into the index package.
type httpStatuser interface{ HTTPStatusCode() int }

// isMissing reports whether err is a 403 or 404 from HeadObject (an object that
// is absent or access-denied), as opposed to a transport/other error.
func isMissing(err error) bool {
	var hs httpStatuser
	if errors.As(err, &hs) {
		switch hs.HTTPStatusCode() {
		case http.StatusNotFound, http.StatusForbidden:
			return true
		}
	}
	return false
}

// BuildFromKeys builds an index from an explicit key list. Every key that lacks
// size+mtime is resolved with HeadObject (bounded by Concurrency; the client's
// --no-sign-request etc. flow through the client itself); keys carrying both
// size and mtime skip the HEAD. A key that HeadObject reports as 403/404 is
// "missing": the build fails with an error listing up to ~10 of them unless
// AllowMissing, in which case it is skipped and counted. Any other HeadObject
// error (network, 5xx, …) fails the build regardless of AllowMissing.
//
// The object key HeadObject is issued against is normalizePrefix(Prefix)+Key;
// the stored Entry.Key stays prefix-relative. When size/mtime came from the
// file (no HEAD), the entry's ETagHash is 0.
func BuildFromKeys(ctx context.Context, api s3client.API, opts KeysOptions) (*Index, KeysResult, error) {
	root := normalizePrefix(opts.Prefix)
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 32
	}

	var (
		mu       sync.Mutex
		entries  = make([]Entry, 0, len(opts.Keys))
		missing  []string
		headed   int
		fatalErr error
	)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	setFatal := func(err error) {
		mu.Lock()
		if fatalErr == nil {
			fatalErr = err
			cancel()
		}
		mu.Unlock()
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for _, ke := range opts.Keys {
		if ke.HasMeta {
			// Size and mtime came from the file: no HEAD, ETagHash unknown (0).
			mu.Lock()
			entries = append(entries, Entry{Key: ke.Key, Size: ke.Size, MTime: ke.MTime})
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		ke := ke
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if cctx.Err() != nil {
				return
			}
			obj, err := api.HeadObject(cctx, root+ke.Key)
			if err != nil {
				if isMissing(err) {
					mu.Lock()
					missing = append(missing, ke.Key)
					mu.Unlock()
					return
				}
				setFatal(fmt.Errorf("index: HeadObject %q: %w", root+ke.Key, err))
				return
			}
			mu.Lock()
			headed++
			entries = append(entries, Entry{
				Key:      ke.Key,
				Size:     obj.Size,
				MTime:    obj.LastModified.UnixNano(),
				ETagHash: HashETag(obj.ETag),
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	if fatalErr != nil {
		return nil, KeysResult{}, fatalErr
	}
	res := KeysResult{Headed: headed, Missing: len(missing), MissingKeys: missing}
	if len(missing) > 0 && !opts.AllowMissing {
		return nil, res, missingKeysError(missing)
	}

	bopts := opts.Options
	bopts.Prefix = root
	if bopts.Source == "" {
		bopts.Source = "keys"
	}
	bopts.KeysSHA = opts.KeysSHA
	return Build(entries, bopts), res, nil
}

// missingKeysError formats the "keys not found" build failure, naming up to 10.
func missingKeysError(missing []string) error {
	const max = 10
	shown := missing
	more := 0
	if len(shown) > max {
		shown = shown[:max]
		more = len(missing) - max
	}
	msg := fmt.Sprintf("index: %d key(s) not found or access-denied: %s", len(missing), strings.Join(shown, ", "))
	if more > 0 {
		msg += fmt.Sprintf(" (and %d more)", more)
	}
	msg += "; pass --keys-allow-missing to skip them"
	return errors.New(msg)
}
