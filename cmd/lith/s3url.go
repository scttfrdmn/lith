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

// parseCargoshipManifestArg resolves the --cargoship value to (bucket, key). It
// accepts either a full s3://bucket/key URL or a bare key relative to the mount
// bucket (mountBucket). A key must be non-empty.
func parseCargoshipManifestArg(arg, mountBucket string) (bucket, key string, err error) {
	if strings.HasPrefix(arg, "s3://") {
		bucket, key, err = parseS3URL(arg)
		if err != nil {
			return "", "", err
		}
		if key == "" {
			return "", "", fmt.Errorf("--cargoship URL %q has no manifest key", arg)
		}
		return bucket, key, nil
	}
	if arg == "" {
		return "", "", fmt.Errorf("--cargoship needs a manifest URL or key")
	}
	return mountBucket, strings.TrimPrefix(arg, "/"), nil
}

// newLogger returns an info-level slog logger writing to stderr: text on a
// terminal, JSON otherwise (per design §6).
func newLogger() *slog.Logger { return newLoggerAt(slog.LevelInfo) }

// newLoggerAt is newLogger at an explicit minimum level.
func newLoggerAt(level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if isTerminal(os.Stderr) {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// parseLogLevel maps a --log-level string to a slog.Level, defaulting to info
// for an empty or unrecognized value.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
