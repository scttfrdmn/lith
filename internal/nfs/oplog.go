// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"fmt"
	"log/slog"
	"strings"

	gonfs "github.com/willscott/go-nfs"
)

// opLogger adapts go-nfs's global Logger to two jobs lith wants.
//
//  1. Count the true NFS procedure per request. go-nfs exposes no per-op hook on
//     the Handler/billy interface — GETATTR, LOOKUP and ACCESS all land on
//     billy.Lstat, so counting there conflates them (#247). But its serve loop
//     calls Log.Tracef("request: %v", req) unconditionally, and request.String()
//     renders "RPC #N (nfs.GetAttr)", so the procedure is in the trace message.
//     This is the only place go-nfs uses Tracef, so every Tracef call is one
//     request and the parse is unambiguous.
//  2. Route go-nfs's own error/warn logs into lith's slog. They otherwise go to
//     stderr unstructured (e.g. the benign "No handler for 100227.0" NFS_ACL
//     client probe); through slog they respect --log-level and carry the gateway's
//     structured context.
//
// The dependency on a log-message format is deliberately made visible, not
// silent: a request line that carries no nfs.* procedure (a future format
// change, or a non-NFS request) counts as "unparsed" so a regression surfaces as
// a nonzero counter rather than every op silently dropping to zero.
type opLogger struct {
	log     *slog.Logger
	metrics Metrics
	level   gonfs.LogLevel
}

var _ gonfs.Logger = (*opLogger)(nil)

func newOpLogger(log *slog.Logger, m Metrics) *opLogger {
	if log == nil {
		log = slog.Default()
	}
	return &opLogger{log: log, metrics: m, level: gonfs.TraceLevel}
}

// countRequest increments lith_nfs_ops_total for the procedure named in a
// "request: RPC #N (nfs.Proc)" trace line. A request line with no nfs.*
// procedure (MOUNT-service ops, the NFS_ACL probe, or a changed log format)
// counts as "unparsed".
func (l *opLogger) countRequest(msg string) {
	if l.metrics == nil {
		return
	}
	if op := nfsProcFromTrace(msg); op != "" {
		l.metrics.NFSOp(op)
		return
	}
	l.metrics.NFSOp("unparsed")
}

// nfsProcFromTrace pulls "getattr" out of "...(nfs.GetAttr)". Returns "" when the
// line carries no nfs.* procedure.
func nfsProcFromTrace(msg string) string {
	const marker = "(nfs."
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j <= 0 {
		return ""
	}
	return strings.ToLower(rest[:j])
}

// Tracef is go-nfs's per-request hook (its only Tracef call). We count the
// procedure and do not emit — the trace line itself is noise at lith's levels.
func (l *opLogger) Tracef(format string, args ...interface{}) {
	l.countRequest(fmt.Sprintf(format, args...))
}

func (l *opLogger) Trace(args ...interface{}) { l.countRequest(fmt.Sprint(args...)) }

// Error/Warn/Info/Debug/Print route go-nfs's own logging into slog. Panic and
// Fatal are demoted to Error rather than crashing the process — a logging call
// from a dependency must never take the gateway down (go-nfs never calls them,
// but the interface requires them).
func (l *opLogger) Panic(args ...interface{}) { l.log.Error(fmt.Sprint(args...)) }
func (l *opLogger) Fatal(args ...interface{}) { l.log.Error(fmt.Sprint(args...)) }
func (l *opLogger) Error(args ...interface{}) { l.log.Error(fmt.Sprint(args...)) }
func (l *opLogger) Warn(args ...interface{})  { l.log.Warn(fmt.Sprint(args...)) }
func (l *opLogger) Info(args ...interface{})  { l.log.Info(fmt.Sprint(args...)) }
func (l *opLogger) Debug(args ...interface{}) { l.log.Debug(fmt.Sprint(args...)) }
func (l *opLogger) Print(args ...interface{}) { l.log.Info(fmt.Sprint(args...)) }

func (l *opLogger) Panicf(f string, a ...interface{}) { l.log.Error(fmt.Sprintf(f, a...)) }
func (l *opLogger) Fatalf(f string, a ...interface{}) { l.log.Error(fmt.Sprintf(f, a...)) }
func (l *opLogger) Errorf(f string, a ...interface{}) { l.log.Error(fmt.Sprintf(f, a...)) }
func (l *opLogger) Warnf(f string, a ...interface{})  { l.log.Warn(fmt.Sprintf(f, a...)) }
func (l *opLogger) Infof(f string, a ...interface{})  { l.log.Info(fmt.Sprintf(f, a...)) }
func (l *opLogger) Debugf(f string, a ...interface{}) { l.log.Debug(fmt.Sprintf(f, a...)) }
func (l *opLogger) Printf(f string, a ...interface{}) { l.log.Info(fmt.Sprintf(f, a...)) }

// Level plumbing: lith never gates on go-nfs's level (it decides per method), but
// the interface requires it. GetLevel reports Trace so any future guarded Tracef
// still reaches countRequest.
func (l *opLogger) SetLevel(level gonfs.LogLevel) { l.level = level }
func (l *opLogger) GetLevel() gonfs.LogLevel      { return l.level }

func (l *opLogger) ParseLevel(level string) (gonfs.LogLevel, error) {
	switch strings.ToLower(level) {
	case "panic":
		return gonfs.PanicLevel, nil
	case "fatal":
		return gonfs.FatalLevel, nil
	case "error":
		return gonfs.ErrorLevel, nil
	case "warn":
		return gonfs.WarnLevel, nil
	case "info":
		return gonfs.InfoLevel, nil
	case "debug":
		return gonfs.DebugLevel, nil
	case "trace":
		return gonfs.TraceLevel, nil
	default:
		return gonfs.InfoLevel, fmt.Errorf("nfs: unknown log level %q", level)
	}
}
