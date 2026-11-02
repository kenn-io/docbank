package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    string
		want  slog.Level
		valid bool
	}{
		{in: "debug", want: slog.LevelDebug, valid: true},
		{in: " info ", want: slog.LevelInfo, valid: true},
		{in: "warn", want: slog.LevelWarn, valid: true},
		{in: "warning", want: slog.LevelWarn, valid: true},
		{in: "ERROR", want: slog.LevelError, valid: true},
		{in: "", want: slog.LevelInfo, valid: false},
		{in: "verbose", want: slog.LevelInfo, valid: false},
	}
	for _, tt := range tests {
		got, valid := ParseLevel(tt.in)
		if got != tt.want || valid != tt.valid {
			require.FailNow(t, fmt.Sprintf("ParseLevel(%q) = (%v, %v), want (%v, %v)", tt.in, got, valid, tt.want, tt.valid))
		}
	}
}

func TestAutoFormatUsesTerminalDetector(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	var text bytes.Buffer
	textLogger, textRes, err := NewLogger(Options{
		Stderr:     &text,
		Format:     FormatAuto,
		Level:      "info",
		IsTerminal: func(io.Writer) bool { return true },
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = textRes.Close() }()

	textLogger.Info("hello", "k", "v")
	if textRes.Format != FormatText {
		require.FailNow(fmt.Sprintf("Format = %q, want text", textRes.Format))
	}
	if strings.HasPrefix(strings.TrimSpace(text.String()), "{") {
		require.FailNow(fmt.Sprintf("auto terminal output should be text, got %q", text.String()))
	}

	var jsonBuf bytes.Buffer
	jsonLogger, jsonRes, err := NewLogger(Options{
		Stderr:     &jsonBuf,
		Format:     FormatAuto,
		Level:      "info",
		IsTerminal: func(io.Writer) bool { return false },
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = jsonRes.Close() }()

	jsonLogger.Info("hello", "k", "v")
	if jsonRes.Format != FormatJSON {
		require.FailNow(fmt.Sprintf("Format = %q, want json", jsonRes.Format))
	}
	if !strings.HasPrefix(strings.TrimSpace(jsonBuf.String()), "{") {
		require.FailNow(fmt.Sprintf("auto non-terminal output should be JSON, got %q", jsonBuf.String()))
	}
}

func TestEnvLevelOverride(t *testing.T) {
	t.Setenv("KIT_LOG_LEVEL_TEST", "debug")

	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr:      &stderr,
		Format:      FormatJSON,
		Level:       "error",
		EnvLevelVar: "KIT_LOG_LEVEL_TEST",
	})
	if err != nil {
		require.FailNow(t, err.Error())
	}
	defer func() { _ = res.Close() }()

	if res.Level != slog.LevelDebug {
		require.FailNow(t, fmt.Sprintf("Level = %v, want debug", res.Level))
	}
	logger.Debug("visible")
	if !strings.Contains(stderr.String(), `"msg":"visible"`) {
		require.FailNow(t, fmt.Sprintf("debug record missing: %q", stderr.String()))
	}
}

func TestInvalidEnvLevelIsIgnored(t *testing.T) {
	t.Setenv("KIT_LOG_LEVEL_TEST", "verbose")

	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr:      &stderr,
		Format:      FormatJSON,
		Level:       "warn",
		EnvLevelVar: "KIT_LOG_LEVEL_TEST",
	})
	if err != nil {
		require.FailNow(t, err.Error())
	}
	defer func() { _ = res.Close() }()

	logger.Info("silent")
	if stderr.Len() != 0 {
		require.FailNow(t, fmt.Sprintf("invalid env override should keep warn level, got %q", stderr.String()))
	}
	logger.Warn("shown")
	if !strings.Contains(stderr.String(), `"msg":"shown"`) {
		require.FailNow(t, fmt.Sprintf("warn record missing: %q", stderr.String()))
	}
}

func TestFileFanoutRunIDAndDailyPath(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr:   &stderr,
		Format:   FormatText,
		Level:    "info",
		AddRunID: true,
		RunID:    "run-123",
		File: FileOptions{
			Enabled:         true,
			Dir:             dir,
			DailyFilePrefix: "kit",
		},
		Now: func() time.Time {
			return time.Date(2026, 5, 29, 12, 0, 0, 0, time.FixedZone("test", 2*60*60))
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	logger.Info("hello", "answer", 42)

	if !strings.Contains(stderr.String(), "hello") || !strings.Contains(stderr.String(), "run_id=run-123") {
		require.FailNow(fmt.Sprintf("stderr missing text record or run_id: %q", stderr.String()))
	}

	wantPath := filepath.Join(dir, "kit-2026-05-29.log")
	if res.FilePath != wantPath {
		require.FailNow(fmt.Sprintf("FilePath = %q, want %q", res.FilePath, wantPath))
	}

	record := readSingleJSONRecord(t, res.FilePath)
	if record["msg"] != "hello" {
		require.FailNow(fmt.Sprintf("msg = %v, want hello", record["msg"]))
	}
	if record["run_id"] != "run-123" {
		require.FailNow(fmt.Sprintf("run_id = %v, want run-123", record["run_id"]))
	}
	if record["answer"] != float64(42) {
		require.FailNow(fmt.Sprintf("answer = %v, want 42", record["answer"]))
	}
}

func TestFileRotationAndRetention(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "kit-2026-05-29.log")
	writeFile(t, path, strings.Repeat("x", 20))
	writeFile(t, path+".1", "old-1")
	writeFile(t, path+".2", "old-2")
	writeFile(t, path+".3", "stale")

	_, res, err := NewLogger(Options{
		Stderr: &bytes.Buffer{},
		Format: FormatText,
		Level:  "info",
		File: FileOptions{
			Enabled:         true,
			Dir:             dir,
			DailyFilePrefix: "kit",
			MaxBytes:        10,
			KeepRotated:     2,
		},
		Now: func() time.Time {
			return time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	if got := readFile(t, path+".1"); got != strings.Repeat("x", 20) {
		require.FailNow(fmt.Sprintf(".1 = %q, want rotated current file", got))
	}
	if got := readFile(t, path+".2"); got != "old-1" {
		require.FailNow(fmt.Sprintf(".2 = %q, want previous .1", got))
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		require.FailNow(fmt.Sprintf(".3 should be pruned, stat err = %v", err))
	}
}

func TestFileRotatesWhileLogging(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr: &stderr,
		Format: FormatText,
		Level:  "info",
		File: FileOptions{
			Enabled:         true,
			Dir:             dir,
			DailyFilePrefix: "kit",
			MaxBytes:        220,
			KeepRotated:     2,
		},
		Now: func() time.Time {
			return time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	logger.Info("first", "payload", strings.Repeat("a", 120))
	logger.Info("second", "payload", strings.Repeat("b", 120))

	rotatedPath := res.FilePath + ".1"
	rotated := readFile(t, rotatedPath)
	if !strings.Contains(rotated, `"msg":"first"`) {
		require.FailNow(fmt.Sprintf("rotated file missing first record: %q", rotated))
	}
	current := readFile(t, res.FilePath)
	if !strings.Contains(current, `"msg":"second"`) {
		require.FailNow(fmt.Sprintf("current file missing second record: %q", current))
	}
	if strings.Contains(current, `"msg":"first"`) {
		require.FailNow(fmt.Sprintf("current file should have rotated before second record: %q", current))
	}
}

func TestFileLoggingFailureDegradesToStderrOnly(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-dir")
	writeFile(t, notDir, "plain file")

	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr: &stderr,
		Format: FormatText,
		Level:  "info",
		File: FileOptions{
			Enabled: true,
			Dir:     filepath.Join(notDir, "logs"),
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	if res.FilePath != "" || res.FileHandler != nil {
		require.FailNow(fmt.Sprintf("file logging should be unavailable, got path %q handler %T", res.FilePath, res.FileHandler))
	}
	if !strings.Contains(stderr.String(), "could not prepare log file") {
		require.FailNow(fmt.Sprintf("stderr missing degradation warning: %q", stderr.String()))
	}

	logger.Info("still-visible")
	if !strings.Contains(stderr.String(), "still-visible") {
		require.FailNow(fmt.Sprintf("stderr-only handler did not log: %q", stderr.String()))
	}
}

func TestFileLoggingFailureUsesConfiguredFormat(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-dir")
	writeFile(t, notDir, "plain file")

	var stderr bytes.Buffer
	_, res, err := NewLogger(Options{
		Stderr: &stderr,
		Format: FormatJSON,
		Level:  "error",
		File: FileOptions{
			Enabled: true,
			Dir:     filepath.Join(notDir, "logs"),
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &record); err != nil {
		require.FailNow(fmt.Sprintf("warning should use JSON format: %v\n%s", err, stderr.String()))
	}
	if record["level"] != "WARN" {
		require.FailNow(fmt.Sprintf("level = %v, want WARN", record["level"]))
	}
	if record["msg"] != "could not prepare log file" {
		require.FailNow(fmt.Sprintf("msg = %v, want file warning", record["msg"]))
	}
	if record["fallback"] != "stderr-only" {
		require.FailNow(fmt.Sprintf("fallback = %v, want stderr-only", record["fallback"]))
	}
	if _, ok := record["error"].(string); !ok {
		require.FailNow(fmt.Sprintf("error attr missing from warning: %#v", record))
	}
}

func TestFileOnlyLoggerWritesFileAndDiscardWhenUnavailable(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	dir := t.TempDir()
	var stderr bytes.Buffer
	_, res, err := NewLogger(Options{
		Stderr:   &stderr,
		Format:   FormatText,
		Level:    "info",
		AddRunID: true,
		RunID:    "run-file-only",
		File: FileOptions{
			Enabled:         true,
			Dir:             dir,
			DailyFilePrefix: "kit",
		},
		Now: func() time.Time {
			return time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	defer func() { _ = res.Close() }()

	res.FileOnlyLogger().Info("file-only")
	if strings.Contains(stderr.String(), "file-only") {
		require.FailNow(fmt.Sprintf("file-only logger wrote to stderr: %q", stderr.String()))
	}

	record := readSingleJSONRecord(t, res.FilePath)
	if record["msg"] != "file-only" || record["run_id"] != "run-file-only" {
		require.FailNow(fmt.Sprintf("unexpected file-only record: %#v", record))
	}

	var discard bytes.Buffer
	_, noFileRes, err := NewLogger(Options{
		Stderr: &discard,
		Format: FormatText,
		Level:  "info",
	})
	if err != nil {
		require.FailNow(err.Error())
	}
	noFileRes.FileOnlyLogger().Info("discarded")
	if discard.Len() != 0 {
		require.FailNow(fmt.Sprintf("discard file-only logger wrote to stderr: %q", discard.String()))
	}
}

func TestAddSource(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	logger, res, err := NewLogger(Options{
		Stderr:    &stderr,
		Format:    FormatJSON,
		Level:     "info",
		AddSource: true,
	})
	if err != nil {
		require.FailNow(t, err.Error())
	}
	defer func() { _ = res.Close() }()

	logger.Info("source")
	if !strings.Contains(stderr.String(), `"source"`) {
		require.FailNow(t, fmt.Sprintf("source annotation missing: %q", stderr.String()))
	}
}

func readSingleJSONRecord(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	record := make(map[string]any)
	if err := json.Unmarshal(lines[len(lines)-1], &record); err != nil {
		require.FailNow(t, fmt.Sprintf("unmarshal %q: %v", lines[len(lines)-1], err))
	}
	return record
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.FailNow(t, err.Error())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	return string(data)
}
