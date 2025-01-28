package log

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorWhite  = "\033[37m"
)

// PrettyHandler provides a more readable log format
type PrettyHandler struct {
	l      *log.Logger
	module string
}

type Logger struct {
	*slog.Logger
}

func NewLogger(module string) Logger {
	prettyHandler := &PrettyHandler{
		l: log.New(os.Stdout, "", 0),
	}
	return Logger{slog.New(prettyHandler).With("module", module)}
}

// Implement the required slog.Handler interface methods
func (h *PrettyHandler) Enabled(ctx context.Context, level slog.Level) bool {
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
	var levelColor string
	level := r.Level.String()

	switch r.Level {
	case slog.LevelDebug:
		levelColor = colorBlue
	case slog.LevelInfo:
		levelColor = colorWhite
	case slog.LevelWarn:
		levelColor = colorYellow
	case slog.LevelError:
		levelColor = colorRed
	default:
		levelColor = colorReset
	}

	component := h.module // use whatever was stored in WithAttrs
	var attrs []string

	r.Attrs(func(a slog.Attr) bool {
		// Skip time attribute
		if a.Key == slog.TimeKey {
			return true
		}
		attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
		return true
	})

	if component == "" {
		component = "unknown"
	}

	// Format the log entry with simple space separation
	logLine := fmt.Sprintf("(%s) %s %s", component, r.Message, strings.Join(attrs, " "))
	h.l.Printf("%s%-5s%s %s", levelColor, level, colorReset, logLine)
	return nil
}
