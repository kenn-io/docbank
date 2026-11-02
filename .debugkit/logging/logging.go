// Package logging builds reusable slog loggers for Go CLIs and services.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"go.kenn.io/kit/atomicfile"
)

// Format selects the serialization used by the stderr handler.
type Format string

const (
	// FormatAuto selects text for TTY writers and JSON otherwise.
	FormatAuto Format = "auto"
	// FormatJSON writes structured JSON records.
	FormatJSON Format = "json"
	// FormatText writes human-readable text records.
	FormatText Format = "text"
	// FormatGCP writes JSON records in the shape Google Cloud Logging parses.
	// See NewCloudLoggingHandler.
	FormatGCP Format = "gcp"
)

const (
	defaultRunIDKey        = "run_id"
	defaultDailyFilePrefix = "app"
	defaultMaxFileBytes    = 50 * 1024 * 1024
	defaultKeepRotated     = 5
)

// TerminalDetector reports whether w is attached to a terminal.
type TerminalDetector func(w io.Writer) bool

// Clock returns the current time.
type Clock func() time.Time

// Options controls how BuildHandler constructs a slog handler.
type Options struct {
	// Stderr receives interactive log output. Nil defaults to os.Stderr.
	Stderr io.Writer

	// Format controls the stderr handler. Empty means auto.
	Format Format

	// Level is the configured level. Empty and unknown values resolve to info.
	Level string

	// EnvLevelVar, when non-empty, names an environment variable that can
	// override Level. Invalid env values are ignored.
	EnvLevelVar string

	// AddSource enables slog source location annotations.
	AddSource bool

	// AddRunID attaches a per-run id attribute to every record. If RunID is
	// empty, BuildHandler generates one.
	AddRunID bool

	// RunID is the run id to attach. A non-empty RunID implies AddRunID.
	RunID string

	// RunIDKey is the attribute key for RunID. Empty means "run_id".
	RunIDKey string

	// File enables optional JSON file logging.
	File FileOptions

	// Now is injected for deterministic daily file names in tests.
	Now Clock

	// IsTerminal is injected for deterministic auto-format tests.
	IsTerminal TerminalDetector
}

// FileOptions controls optional JSON file logging.
type FileOptions struct {
	// Enabled turns on file logging. File logging failures degrade to
	// stderr-only logging with a warning on stderr.
	Enabled bool

	// Dir is the directory for daily log files. Used when Path is empty.
	Dir string

	// Path, when non-empty, overrides the daily file path.
	Path string

	// DailyFilePrefix controls daily file names:
	// <DailyFilePrefix>-YYYY-MM-DD.log. Empty means "app".
	DailyFilePrefix string

	// MaxBytes rotates the log file when the active file would grow beyond
	// this size. Zero means 50 MiB.
	MaxBytes int64

	// KeepRotated is the number of rotated files to retain. Zero means 5.
	KeepRotated int
}

// Result describes the handler BuildHandler constructed.
type Result struct {
	Handler slog.Handler

	// FileHandler is the JSON-to-disk handler without stderr fanout.
	FileHandler slog.Handler

	Level  slog.Level
	Format Format

	// RunID is empty when run id attachment is disabled.
	RunID    string
	RunIDKey string

	// FilePath is empty when file logging is disabled or unavailable.
	FilePath string

	closers []func() error
}

// ParseLevel maps a user-facing level string to slog.Level.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// IsValidLevel reports whether s is a recognized log level.
func IsValidLevel(s string) bool {
	_, ok := ParseLevel(s)
	return ok
}

// ParseFormat maps a user-facing format string to Format.
func ParseFormat(s string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return FormatAuto, true
	case "json":
		return FormatJSON, true
	case "text":
		return FormatText, true
	case "gcp":
		return FormatGCP, true
	default:
		return "", false
	}
}

// IsTerminalWriter reports whether w exposes a terminal file descriptor.
func IsTerminalWriter(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}

// NewLogger constructs a slog.Logger and returns the underlying Result.
func NewLogger(opts Options) (*slog.Logger, *Result, error) {
	res, err := BuildHandler(opts)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(res.Handler), res, nil
}

// BuildHandler constructs a slog handler from opts.
func BuildHandler(opts Options) (*Result, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	format, ok := ParseFormat(string(opts.Format))
	if !ok {
		return nil, fmt.Errorf("logging: unknown format %q", opts.Format)
	}
	if format == FormatAuto {
		isTerminal := opts.IsTerminal
		if isTerminal == nil {
			isTerminal = IsTerminalWriter
		}
		if isTerminal(stderr) {
			format = FormatText
		} else {
			format = FormatJSON
		}
	}

	level, _ := ParseLevel(opts.Level)
	if opts.EnvLevelVar != "" {
		if value := os.Getenv(opts.EnvLevelVar); value != "" {
			if envLevel, valid := ParseLevel(value); valid {
				level = envLevel
			}
		}
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     level,
		AddSource: opts.AddSource,
	}

	stderrHandler := newHandler(stderr, format, handlerOpts)
	handlers := []slog.Handler{stderrHandler}

	res := &Result{
		Level:  level,
		Format: format,
	}

	if opts.File.Enabled {
		filePath, fileHandler, closeFile, err := buildFileHandler(opts.File, opts.Now, handlerOpts)
		if err != nil {
			target := filePath
			if target == "" {
				target = opts.File.Path
			}
			if target == "" {
				target = opts.File.Dir
			}
			if target == "" {
				target = "log file"
			}
			emitFileLoggingWarning(stderrHandler, target, err)
		} else {
			handlers = append(handlers, fileHandler)
			res.FileHandler = fileHandler
			res.FilePath = filePath
			res.closers = append(res.closers, closeFile)
		}
	}

	var handler slog.Handler
	if len(handlers) == 1 {
		handler = handlers[0]
	} else {
		handler = newMultiHandler(handlers...)
	}

	if opts.AddRunID || opts.RunID != "" {
		runID := opts.RunID
		if runID == "" {
			runID = newRunID()
		}
		runIDKey := opts.RunIDKey
		if runIDKey == "" {
			runIDKey = defaultRunIDKey
		}
		handler = handler.WithAttrs([]slog.Attr{slog.String(runIDKey, runID)})
		res.RunID = runID
		res.RunIDKey = runIDKey
	}

	res.Handler = handler
	return res, nil
}

// FileOnlyLogger returns a logger that writes only to the JSON file handler.
// It returns a discard logger when file logging is disabled or unavailable.
func (r *Result) FileOnlyLogger() *slog.Logger {
	if r == nil || r.FileHandler == nil {
		return slog.New(discardHandler{})
	}

	handler := r.FileHandler
	if r.RunID != "" {
		runIDKey := r.RunIDKey
		if runIDKey == "" {
			runIDKey = defaultRunIDKey
		}
		handler = handler.WithAttrs([]slog.Attr{slog.String(runIDKey, r.RunID)})
	}
	return slog.New(handler)
}

// Close releases resources held by Result. It is safe to call multiple times.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}

	var err error
	for _, closer := range slices.Backward(r.closers) {
		if closeErr := closer(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	r.closers = nil
	return err
}

func newHandler(w io.Writer, format Format, opts *slog.HandlerOptions) slog.Handler {
	switch format {
	case FormatJSON:
		return slog.NewJSONHandler(w, opts)
	case FormatGCP:
		return NewCloudLoggingHandler(w, opts)
	default:
		return slog.NewTextHandler(w, opts)
	}
}

func emitFileLoggingWarning(handler slog.Handler, target string, err error) {
	ctx := context.Background()
	record := slog.NewRecord(time.Now(), slog.LevelWarn, "could not prepare log file", 0)
	record.AddAttrs(
		slog.String("path", target),
		slog.String("error", err.Error()),
		slog.String("fallback", "stderr-only"),
	)
	_ = handler.Handle(ctx, record)
}

func buildFileHandler(
	opts FileOptions,
	now Clock,
	handlerOpts *slog.HandlerOptions,
) (string, slog.Handler, func() error, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxFileBytes
	}
	if opts.KeepRotated <= 0 {
		opts.KeepRotated = defaultKeepRotated
	}
	if now == nil {
		now = time.Now
	}

	path := opts.Path
	if path == "" {
		if opts.Dir == "" {
			return "", nil, nil, errors.New("missing log directory or file path")
		}
		prefix := opts.DailyFilePrefix
		if prefix == "" {
			prefix = defaultDailyFilePrefix
		}
		path = filepath.Join(
			opts.Dir,
			fmt.Sprintf("%s-%s.log", prefix, now().UTC().Format("2006-01-02")),
		)
	}

	file, err := openRotatingFile(path, opts.MaxBytes, opts.KeepRotated)
	if err != nil {
		return path, nil, nil, err
	}

	handler := slog.NewJSONHandler(file, handlerOpts)
	return path, handler, file.Close, nil
}

type rotatingFile struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	keepRotated int
	file        *os.File
	size        int64
}

func openRotatingFile(path string, maxBytes int64, keepRotated int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir log dir: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxBytes {
		if err := rotate(path, keepRotated); err != nil {
			return nil, fmt.Errorf("rotate log file: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat log file: %w", err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	fi, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat open log file: %w", err)
	}
	return &rotatingFile{
		path:        path,
		maxBytes:    maxBytes,
		keepRotated: keepRotated,
		file:        file,
		size:        fi.Size(),
	}, nil
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return 0, os.ErrClosed
	}
	if f.size > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

func (f *rotatingFile) rotateLocked() error {
	if err := f.file.Close(); err != nil {
		return err
	}
	f.file = nil

	if err := rotate(f.path, f.keepRotated); err != nil {
		reopened, openErr := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr == nil {
			f.file = reopened
			if fi, statErr := reopened.Stat(); statErr == nil {
				f.size = fi.Size()
			}
		}
		return err
	}

	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file after rotation: %w", err)
	}
	f.file = file
	f.size = 0
	return nil
}

func rotate(path string, keep int) error {
	for i := keep; i >= 1; i-- {
		src := path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", path, i-1)
		}
		dst := fmt.Sprintf("%s.%d", path, i)

		if i == keep {
			_ = os.Remove(dst)
		}
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := atomicfile.Replace(src, dst); err != nil {
			return err
		}
	}
	return pruneRotated(path, keep)
}

func pruneRotated(path string, keep int) error {
	matches, err := filepath.Glob(path + ".*")
	if err != nil {
		return err
	}
	for _, match := range matches {
		suffix := strings.TrimPrefix(match, path+".")
		n, err := strconv.Atoi(suffix)
		if err != nil || n <= keep {
			continue
		}
		if err := os.Remove(match); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func newRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

type multiHandler struct {
	children []slog.Handler
}

func newMultiHandler(children ...slog.Handler) slog.Handler {
	copied := make([]slog.Handler, len(children))
	copy(copied, children)
	return &multiHandler{children: copied}
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range m.children {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, record slog.Record) error {
	var err error
	for _, handler := range m.children {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if handleErr := handler.Handle(ctx, record.Clone()); handleErr != nil {
			err = errors.Join(err, handleErr)
		}
	}
	return err
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, len(m.children))
	for i, handler := range m.children {
		children[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{children: children}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, len(m.children))
	for i, handler := range m.children {
		children[i] = handler.WithGroup(name)
	}
	return &multiHandler{children: children}
}
