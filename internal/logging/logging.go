// Package logging captures structured logs, stderr writes, and panics to a
// per-session log file under ./logs/. The TUI takes over the terminal during
// a run, so neither stdout nor stderr is usable for diagnostics; everything
// flows through this package into the file.
//
// On top of log/slog:
//
//	log := logging.For("llm")
//	log.Info("request sent", "model", cfg.Model, "msgs", n, "tools", t)
//	log.Error("stream failed", "err", err)
//
// Output looks like:
//
//	07:44:12.103 INFO  llm: request sent           model=deepseek-chat msgs=3 tools=6
//	07:44:12.480 INFO  llm: stream first chunk     latency_ms=377
//
// The package also keeps the older Errorf/Infof helpers and the Recover()
// goroutine-guard so existing call sites keep compiling.
package logging

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Session owns the log file and the slog handler writing into it. A nil
// *Session is a valid no-op receiver.
type Session struct {
	file  *os.File
	path  string
	level slog.LevelVar
	mu    sync.Mutex
}

var (
	mu      sync.RWMutex
	current *Session
)

// Init creates baseDir/logs/, opens harness-<timestamp>.log inside it,
// redirects os.Stderr (and, where supported, fd 2) at the file, installs a
// slog.TextHandler at the requested level as the process default, and stores
// the session globally. Failures are returned but never panic; the package
// then degrades to no-ops.
//
// levelStr accepts "debug", "info", "warn", "error" (case-insensitive). An
// empty or unrecognised value defaults to info.
func Init(baseDir, levelStr string) (*Session, error) {
	if baseDir == "" {
		baseDir = "."
	}
	logsDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: mkdir %s: %w", logsDir, err)
	}
	name := fmt.Sprintf("harness-%s.log", time.Now().Format("20060102-150405"))
	path := filepath.Join(logsDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logging: open %s: %w", path, err)
	}

	if err := redirectStderr(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logging: redirect stderr: %w", err)
	}

	s := &Session{file: f, path: path}
	s.level.Set(parseLevel(levelStr))

	handler := slog.NewTextHandler(f, &slog.HandlerOptions{
		Level:       &s.level,
		ReplaceAttr: replaceAttr,
	})
	slog.SetDefault(slog.New(handler))

	// Also route the standard `log` package at the file, so anything that
	// uses log.Printf lands here too.
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	mu.Lock()
	current = s
	mu.Unlock()

	slog.Info("session started",
		"component", "logging",
		"log_path", path,
		"level", s.level.Level().String(),
		"pid", os.Getpid(),
	)
	return s, nil
}

// Logger is a thin wrapper that resolves slog.Default() lazily on every
// call. We need this because package-level `var log = logging.For("...")`
// declarations evaluate during package init — long before main() runs and
// calls Init(). If For returned a *slog.Logger captured at that moment, the
// component loggers would keep using the built-in default handler (which
// routes through the standard log package and ignores our level setting)
// even after Init swapped in a TextHandler.
type Logger struct {
	component string
}

// For returns a *Logger pre-tagged with the given component name. Safe to
// assign at package scope: `var log = logging.For("llm")`. The actual slog
// handler is looked up at each log call, so the logger automatically picks
// up whatever Init configured.
func For(component string) *Logger {
	return &Logger{component: component}
}

// base returns a slog.Logger backed by the current default handler, with
// the component attr applied. Allocating a Logger per call is cheap (it's a
// struct of a *Handler and a small attr slice); we're nowhere near a hot
// path.
func (l *Logger) base() *slog.Logger {
	return slog.Default().With("component", l.component)
}

// Debug, Info, Warn, Error mirror the *slog.Logger API so existing call sites
// (e.g. log.Info("msg", "key", val)) work unchanged.
func (l *Logger) Debug(msg string, args ...any) { l.base().Debug(msg, args...) }
func (l *Logger) Info(msg string, args ...any)  { l.base().Info(msg, args...) }
func (l *Logger) Warn(msg string, args ...any)  { l.base().Warn(msg, args...) }
func (l *Logger) Error(msg string, args ...any) { l.base().Error(msg, args...) }

// SetLevel changes the active log level at runtime. Useful if you ever want
// to flip to debug from a hot-reload hook.
func SetLevel(levelStr string) {
	s := Current()
	if s == nil {
		return
	}
	s.level.Set(parseLevel(levelStr))
}

// Current returns the active session, or nil if Init was never called or
// failed.
func Current() *Session {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Path returns the log file path, or "" for a nil session.
func (s *Session) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Writer exposes the underlying *os.File so callers can pass it to APIs that
// take an io.Writer (e.g. as a ProgressWriter). Returns io.Discard for nil.
// Writes go directly to the file without slog framing — use this for streams
// that already have their own format (subprocess stdout, docker pull progress).
func (s *Session) Writer() io.Writer {
	if s == nil || s.file == nil {
		return io.Discard
	}
	return s.file
}

// Recover is intended to be deferred from goroutines the harness owns. It
// writes the panic value and a full stack trace to the log via slog, then
// re-panics so the process still terminates with the original failure.
// `where` is a short label identifying the goroutine (e.g. "agent.send").
func Recover(where string) {
	r := recover()
	if r == nil {
		return
	}
	slog.Error("goroutine panic",
		"component", "panic",
		"where", where,
		"value", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()),
	)
	if s := Current(); s != nil && s.file != nil {
		_ = s.file.Sync()
	}
	panic(r)
}

// Close flushes a footer and closes the log file. Stderr remains redirected;
// there is no clean way to restore the original handle once a TUI has run.
func (s *Session) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	slog.Info("session ended", "component", "logging")
	return s.file.Close()
}

// replaceAttr customises slog.TextHandler output:
//   - Shorten the time key to "HH:MM:SS.mmm" (we don't need the date — every
//     log file is per-session, and the session's RFC3339 start time is in the
//     session-started beacon).
//   - Move the level into a fixed-width form ("INFO ", "WARN ") so columns
//     line up when eyeballing the file.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		t, ok := a.Value.Any().(time.Time)
		if !ok {
			return a
		}
		return slog.Attr{Key: "t", Value: slog.StringValue(t.Format("15:04:05.000"))}
	case slog.LevelKey:
		lvl, ok := a.Value.Any().(slog.Level)
		if !ok {
			return a
		}
		return slog.Attr{Key: slog.LevelKey, Value: slog.StringValue(fmt.Sprintf("%-5s", lvl.String()))}
	}
	return a
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
