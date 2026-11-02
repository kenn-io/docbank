# logging

Package `logging` centralizes the shared `slog` setup used by kenn-io Go CLIs
and services.

It supports:

- `json`, `text`, and TTY-aware `auto` stderr formatting.
- `gcp` stderr formatting: JSON that Google Cloud Logging parses into
  severity, message, timestamp, source location, and OpenTelemetry trace and
  span fields. It only formats output, so it needs no Google Cloud client or
  credentials. `NewCloudLoggingHandler` builds the same handler directly.
- `debug`, `info`, `warn` / `warning`, and `error` levels.
- Optional environment level override with a caller-supplied env var name.
- Optional JSON file logging with daily paths, size rotation, retained rotated
  files, and stderr/file fanout.
- Optional per-run id attributes and a file-only logger for TUI commands.

## Migration Notes

For simple stderr-only logging, replace private level parsing and TTY selection
with `logging.NewLogger`. Pass the existing stderr writer, `Format`, `Level`,
`AddSource`, and any caller-owned `EnvLevelVar`. Leave `File` and `AddRunID`
unset to preserve no-file, no-run-id behavior.

For CLI file logging, map the existing config into `logging.Options` with
`AddRunID: true`, the stderr writer, the configured level, and
`FileOptions{Enabled: true, Dir: logsDir, DailyFilePrefix: appName,
MaxBytes: maxFileBytes, KeepRotated: keepRotated}`. Commands that enter a TUI or
alternate screen should continue to swap to `Result.FileOnlyLogger()` and call
`Result.Close()` during shutdown.
