package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

type multi struct{ hs []slog.Handler }

func (m multi) Enabled(c context.Context, l slog.Level) bool {
	for _, h := range m.hs {
		if h.Enabled(c, l) {
			return true
		}
	}
	return false
}
func (m multi) Handle(c context.Context, r slog.Record) error {
	for _, h := range m.hs {
		if h.Enabled(c, r.Level) {
			if e := h.Handle(c, r); e != nil {
				return e
			}
		}
	}
	return nil
}
func (m multi) WithAttrs(a []slog.Attr) slog.Handler {
	n := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		n[i] = h.WithAttrs(a)
	}
	return multi{n}
}
func (m multi) WithGroup(s string) slog.Handler {
	n := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		n[i] = h.WithGroup(s)
	}
	return multi{n}
}
func level(v string) (slog.Level, error) {
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q", v)
}
func Setup(file, fileLevel string, debug bool, console io.Writer) (func() error, error) {
	l, e := level(fileLevel)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if e != nil {
		return nil, fmt.Errorf("open log file: %w", e)
	}
	hs := []slog.Handler{slog.NewTextHandler(f, &slog.HandlerOptions{Level: l})}
	if debug {
		hs = append(hs, slog.NewTextHandler(console, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	slog.SetDefault(slog.New(multi{hs}))
	return f.Close, nil
}
