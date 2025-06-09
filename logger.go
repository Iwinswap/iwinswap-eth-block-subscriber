package subscriber

import "log/slog"

type Logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}

type DefaultLogger struct {
}

func (logger *DefaultLogger) Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

func (logger *DefaultLogger) Error(msg string, args ...any) {
	slog.Error(msg, args...)
}

func (logger *DefaultLogger) Warn(msg string, args ...any) {
	slog.Warn(msg, args...)
}

func (logger *DefaultLogger) Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}
