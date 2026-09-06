// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// parseS3URL splits an s3://bucket[/prefix] URL into its bucket and prefix.
// The prefix has no leading slash and is "" when absent.
func parseS3URL(raw string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(raw, "s3://")
	if !ok {
		return "", "", fmt.Errorf("expected an s3://bucket[/prefix] URL, got %q", raw)
	}
	bucket, prefix, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("missing bucket in %q", raw)
	}
	return bucket, prefix, nil
}

// newLogger returns a slog logger writing to stderr: text on a terminal, JSON
// otherwise (per design §6).
func newLogger() *slog.Logger {
	var h slog.Handler
	if isTerminal(os.Stderr) {
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})
	} else {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{})
	}
	return slog.New(h)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
