package posthog

import (
	"context"
	"fmt"
	"log/slog"
)

// sdkLogger preserves the SDK's log levels in the application's slog output.
type sdkLogger struct{ logger *slog.Logger }

func (l sdkLogger) Debugf(format string, args ...any) {
	if l.logger.Enabled(context.Background(), slog.LevelDebug) {
		l.logger.Debug(fmt.Sprintf(format, args...))
	}
}

func (l sdkLogger) Logf(format string, args ...any) { l.logger.Info(fmt.Sprintf(format, args...)) }

func (l sdkLogger) Warnf(format string, args ...any) { l.logger.Warn(fmt.Sprintf(format, args...)) }

func (l sdkLogger) Errorf(format string, args ...any) { l.logger.Error(fmt.Sprintf(format, args...)) }
