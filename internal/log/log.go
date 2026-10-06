package log

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorWhite  = "\033[37m"
	colorGray   = "\033[90m"
)

// Custom log level for verbose output (between Debug and Info)
const LevelVerbose = slog.LevelDebug + 1

var (
	debugEnabled   bool
	verboseEnabled bool
	jsonFormat     bool
)

func init() {
	debugEnabled = os.Getenv("LOG_DEBUG") == "1" || os.Getenv("LOG_DEBUG") == "true"
	verboseEnabled = os.Getenv("LOG_VERBOSE") == "1" || os.Getenv("LOG_VERBOSE") == "true"
	// LOG_FORMAT=json selects a structured slog JSON handler (one object per line,
	// parseable in Loki); any other value (incl. unset / "pretty") keeps the
	// human-readable PrettyHandler. CONTRACT §2.4.
	jsonFormat = strings.EqualFold(os.Getenv("LOG_FORMAT"), "json")
}

// out is where every Logger writes; nil means os.Stdout, resolved at write
// time. MCP stdio mode points it at os.Stderr (SetOutput) because stdout
// carries the JSON-RPC stream there.
var out atomic.Pointer[io.Writer]

// SetOutput redirects every Logger — including ones already created — to w.
// A nil w restores the default (os.Stdout).
func SetOutput(w io.Writer) {
	if w == nil {
		out.Store(nil)
		return
	}
	out.Store(&w)
}

// output is the io.Writer loggers hold: it forwards to the current target.
type output struct{}

func (output) Write(p []byte) (int, error) {
	if w := out.Load(); w != nil {
		return (*w).Write(p)
	}
	return os.Stdout.Write(p)
}

// traceHandler adds trace_id and span_id to records logged with a context that
// carries a valid OpenTelemetry span (logger.InfoContext(ctx, …) inside a
// span), so a JSON log line joins its trace in the backend. Records without a
// span are unchanged.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// levelVar returns the minimum slog level for the current debug setting. The
// custom verbose level sits just above Debug; when neither debug nor verbose is
// on, Info is the floor. Used by the JSON handler (the PrettyHandler enforces the
// debug/verbose gates itself in Enabled).
func minLevel() slog.Level {
	if debugEnabled {
		return slog.LevelDebug
	}
	if verboseEnabled {
		return LevelVerbose
	}
	return slog.LevelInfo
}

// PrettyHandler provides a more readable log format
type PrettyHandler struct {
	l        *log.Logger
	module   string
	logLevel slog.Level
}

type Logger struct {
	*slog.Logger
}

// Verbose logs a message at verbose level (only shown when LOG_VERBOSE=1)
func (l Logger) Verbose(msg string, args ...any) {
	l.Log(context.Background(), LevelVerbose, msg, args...)
}

func NewLogger(module string) Logger {
	logLevel := slog.LevelInfo
	if debugEnabled {
		logLevel = slog.LevelDebug
	}

	// LOG_FORMAT=json: emit structured slog JSON (one object per line) to stdout
	// for Loki, instead of the ANSI PrettyHandler. It honors the same debug/verbose
	// gates (via minLevel) and renders the custom Verbose level with a readable
	// name. The module attribute is carried the same way as the pretty path.
	if jsonFormat {
		h := slog.NewJSONHandler(output{}, &slog.HandlerOptions{
			Level: minLevel(),
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.LevelKey {
					if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelVerbose {
						a.Value = slog.StringValue("VERBOSE")
					}
				}
				return a
			},
		})
		return Logger{slog.New(traceHandler{h}).With("module", module)}
	}

	prettyHandler := &PrettyHandler{
		l:        log.New(output{}, "", 0),
		module:   module,
		logLevel: logLevel,
	}
	return Logger{slog.New(prettyHandler).With("module", module)}
}

// Implement the required slog.Handler interface methods
func (h *PrettyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level == slog.LevelDebug {
		return debugEnabled
	}
	if level == LevelVerbose {
		return verboseEnabled
	}
	return true
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newH := *h
	for _, a := range attrs {
		if a.Key == "module" {
			newH.module = a.Value.String()
		}
	}
	return &newH
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	return h
}

func (h *PrettyHandler) Handle(ctx context.Context, r slog.Record) error {
	var levelColor, levelSymbol string

	switch r.Level {
	case slog.LevelDebug:
		levelColor = colorBlue
		levelSymbol = "•"
	case LevelVerbose:
		levelColor = colorGray
		levelSymbol = "…"
	case slog.LevelInfo:
		levelColor = colorWhite
		levelSymbol = "→"
	case slog.LevelWarn:
		levelColor = colorYellow
		levelSymbol = "!"
	case slog.LevelError:
		levelColor = colorRed
		levelSymbol = "✗"
	default:
		levelColor = colorReset
		levelSymbol = "•"
	}

	component := h.module
	if component == "" {
		component = "app"
	}

	var attrs []string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == slog.TimeKey || a.Key == "module" {
			return true
		}

		key := a.Key
		value := a.Value.Any()

		// Format specific keys for better readability
		switch key {
		case "error":
			attrs = append(attrs, fmt.Sprintf("%s%s%s: %v", colorRed, key, colorReset, value))
		case "count", "total_books", "total_audio_files", "processed_books", "processed_chapters", "total_files", "chapter_index", "num_gc", "enqueued", "skipped":
			attrs = append(attrs, fmt.Sprintf("%s%s%s=%v", colorBlue, key, colorReset, value))
		case "title", "author", "chapter_name", "file", "path", "status":
			attrs = append(attrs, fmt.Sprintf("%s%s%s=%q", colorWhite, key, colorReset, value))
		case "isbn", "asin", "url", "address", "value":
			attrs = append(attrs, fmt.Sprintf("%s%s%s=%v", colorYellow, key, colorReset, value))
		default:
			attrs = append(attrs, fmt.Sprintf("%s%s%s=%v", colorWhite, key, colorReset, value))
		}
		return true
	})

	// Build the log line with improved formatting
	var logLine strings.Builder

	// Component name with subtle styling
	fmt.Fprintf(&logLine, "%s[%s]%s", colorBlue, component, colorReset)

	// Main message
	fmt.Fprintf(&logLine, " %s", r.Message)

	// Attributes if present
	if len(attrs) > 0 {
		logLine.WriteString(" ")
		logLine.WriteString(strings.Join(attrs, " "))
	}

	// Output with level symbol and color
	h.l.Printf("%s%s%s %s", levelColor, levelSymbol, colorReset, logLine.String())
	return nil
}
