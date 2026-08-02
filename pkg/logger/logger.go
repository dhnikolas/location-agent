package logger

import (
	"fmt"
	"log/slog"
)

type LoggerWrap struct {
	*slog.Logger
}

func (l *LoggerWrap) Error(args ...interface{}) {
	l.Logger.Error(fmt.Sprint(args...))
}

func (l *LoggerWrap) Info(args ...interface{}) {
	l.Logger.Info(fmt.Sprint(args...))
}
