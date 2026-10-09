package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
)

type Config struct {
	Level string `mapstructure:"level"`
}

type Service interface {
	Debug(ctx context.Context, msg string, fields map[string]any)
	Info(ctx context.Context, msg string, fields map[string]any)
	Warn(ctx context.Context, msg string, fields map[string]any)
	Error(ctx context.Context, err error, fields map[string]any)
	WithFields(fields map[string]any) Service
}

type ContextExtractor func(ctx context.Context) map[string]any

type Option func(*service)

func WithContextExtractor(extract ContextExtractor) Option {
	return func(s *service) { s.extract = extract }
}

type service struct {
	log     *slog.Logger
	extract ContextExtractor
}

func New(cfg Config, w io.Writer, opts ...Option) (Service, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, fmt.Errorf("logger: invalid level %q", cfg.Level)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: renameKeys})
	s := &service{log: slog.New(h)}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func (s *service) Debug(ctx context.Context, msg string, fields map[string]any) {
	s.write(ctx, slog.LevelDebug, msg, fields)
}

func (s *service) Info(ctx context.Context, msg string, fields map[string]any) {
	s.write(ctx, slog.LevelInfo, msg, fields)
}

func (s *service) Warn(ctx context.Context, msg string, fields map[string]any) {
	s.write(ctx, slog.LevelWarn, msg, fields)
}

func (s *service) Error(ctx context.Context, err error, fields map[string]any) {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	s.write(ctx, slog.LevelError, msg, fields)
}

func (s *service) WithFields(fields map[string]any) Service {
	return &service{log: s.log.With(toArgs(fields)...), extract: s.extract}
}

func (s *service) write(ctx context.Context, level slog.Level, msg string, fields map[string]any) {
	if !s.log.Enabled(ctx, level) {
		return
	}
	var args []any
	if s.extract != nil {
		args = toArgs(s.extract(ctx))
	}
	s.log.Log(ctx, level, sanitizeString(msg), append(args, toArgs(fields)...)...)
}

func toArgs(fields map[string]any) []any {
	args := make([]any, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		args = append(args, slog.Any(k, sanitizeValue(k, fields[k])))
	}
	return args
}

func renameKeys(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.LevelKey:
		a.Key = "severity"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}
