// log_capture.go — Kernel slog capture: tee stderr text output to a JSONL file sink.
//
// Implements Part (a) of Agent U's kernel-slog-api design — the file-sink/capture
// half. The default logger installed by setupLogger() keeps writing key=value text
// to os.Stderr (backwards-compatibility lock for service managers that capture
// stderr); this file adds a fan-out so the same records are simultaneously
// emitted as structured JSON into <WorkspaceRoot>/.cog/run/kernel.log.jsonl.
//
// Why a teeHandler instead of io.MultiWriter: the two sinks have different
// encodings (text on stderr, JSON in the file). A single slog.Handler cannot
// emit two formats; wrapping two sub-handlers in a Handle() fan-out lets us
// keep the human-readable terminal output while also getting machine-parseable
// on-disk output for the cog_tail_kernel_log MCP tool / GET /v1/kernel-log.
//
// Call order: setupLogger() runs at process start (stderr-only text handler);
// upgradeLoggerWithFileSink(cfg) runs AFTER LoadConfig once the workspace root
// is known. Anything logged between the two goes to stderr only — that window
// is microseconds in practice and those early lines are re-emitted after the
// upgrade via the "config loaded" Info call.
//
// The file handle is opened with O_APPEND and left open for the kernel's
// lifetime; the OS flushes on process exit. If the open fails (disk full,
// permission denied, read-only mount) we log a Warn to stderr and fall back
// to the stderr-only handler. The kernel never crashes over log-sink setup.
package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// Kernel log rotation bounds for the JSONL sink. Exposed as vars so tests can
// shrink them; COG_KERNEL_LOG_MAX_MB overrides the size cap at runtime. With the
// defaults the sink is capped at roughly maxBackups+1 × 64 MB on disk.
var (
	kernelLogMaxBytes   int64 = 64 * 1024 * 1024 // rotate the active file past 64 MB
	kernelLogMaxBackups       = 5                // keep 5 rotated copies (~384 MB total)
)

// DefaultKernelLogPath returns the default per-workspace path for the kernel
// slog JSONL sink. Callers should use cfg.KernelLogPath when non-empty and
// fall back to this helper otherwise.
func DefaultKernelLogPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "run", "kernel.log.jsonl")
}

// teeHandler fans a single slog.Record out to two underlying handlers so the
// kernel can keep emitting human-readable text to stderr while also writing
// structured JSON to a file. WithAttrs / WithGroup derive matching pairs on
// both halves so scoped loggers keep the same fan-out.
type teeHandler struct {
	text slog.Handler // slog.NewTextHandler on os.Stderr
	json slog.Handler // slog.NewJSONHandler on the file sink
}

func (h *teeHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.text.Enabled(ctx, lvl) || h.json.Enabled(ctx, lvl)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	// Best-effort on the stderr side — don't let a text-handler error block
	// the JSON sink (or vice-versa). The JSON sink's error is returned
	// because that's the one callers care about most (read API depends on it).
	_ = h.text.Handle(ctx, r)
	return h.json.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{
		text: h.text.WithAttrs(attrs),
		json: h.json.WithAttrs(attrs),
	}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{
		text: h.text.WithGroup(name),
		json: h.json.WithGroup(name),
	}
}

// newTeeHandler constructs a teeHandler that writes text to textSink and JSON
// to jsonSink at the supplied level. Exported for tests; production callers
// use upgradeLoggerWithFileSink. jsonSink is an io.Writer so production can pass
// a rotatingWriter while tests pass a bytes.Buffer or *os.File.
func newTeeHandler(textSink, jsonSink io.Writer, level slog.Level) *teeHandler {
	opts := &slog.HandlerOptions{Level: level}
	return &teeHandler{
		text: slog.NewTextHandler(textSink, opts),
		json: slog.NewJSONHandler(jsonSink, opts),
	}
}

// upgradeLoggerWithFileSink replaces the default slog logger installed by
// setupLogger() with one that writes text to os.Stderr AND structured JSON to
// <cfg.WorkspaceRoot>/.cog/run/kernel.log.jsonl (or the override path if set).
//
// Must be called AFTER LoadConfig so cfg.WorkspaceRoot is valid. Safe to call
// more than once — each call installs a fresh tee wrapper. Failure to open
// the file sink is logged as a Warn via the stderr-only fallback logger and
// the function returns without altering the default logger.
//
// The sink is a size-rotating writer (see rotating_writer.go): the active file
// is capped at kernelLogMaxBytes and rotated to <path>.1..N, so the sink no
// longer grows without bound. The writer is intentionally never Close()'d; it
// lives for the kernel's process lifetime and the OS flushes on exit.
func upgradeLoggerWithFileSink(cfg *Config) {
	if cfg == nil {
		return
	}

	level := slog.LevelInfo
	if os.Getenv("COG_LOG_DEBUG") != "" {
		level = slog.LevelDebug
	}

	path := cfg.KernelLogPath
	if path == "" {
		path = DefaultKernelLogPath(cfg.WorkspaceRoot)
	}

	maxBytes := kernelLogMaxBytes
	if v := os.Getenv("COG_KERNEL_LOG_MAX_MB"); v != "" {
		if mb, perr := strconv.ParseInt(v, 10, 64); perr == nil && mb > 0 {
			maxBytes = mb * 1024 * 1024
		}
	}

	w, err := newRotatingWriter(path, maxBytes, kernelLogMaxBackups)
	if err != nil {
		slog.Warn("kernel log: cannot open sink; continuing with stderr-only logger",
			"path", path, "err", err)
		return
	}
	// Intentionally not closed — lives for kernel lifetime.

	tee := newTeeHandler(os.Stderr, w, level)
	slog.SetDefault(slog.New(tee))
	slog.Info("kernel log: file sink active", "path", path, "max_bytes", maxBytes, "max_backups", kernelLogMaxBackups)
}
