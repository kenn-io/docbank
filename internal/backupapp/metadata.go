package backupapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/store"
)

// MetadataFormat is the stable Kit manifest identifier for Docbank's
// deterministic logical metadata stream.
const MetadataFormat = "docbank-metadata-jsonl-v1"

// MetadataSource pins one Docbank transaction for JSONL, content membership,
// and fidelity statistics. OpenMetadata performs a counting pass and then
// streams a second deterministic pass without materializing the JSONL.
type MetadataSource struct{ metadata *store.Store }

var _ backup.MetadataSource = (*MetadataSource)(nil)

func NewMetadataSource(metadata *store.Store) *MetadataSource {
	return &MetadataSource{metadata: metadata}
}

func (*MetadataSource) Format() string { return MetadataFormat }

func (s *MetadataSource) OpenSnapshot(ctx context.Context) (backup.MetadataSnapshot, error) {
	if s == nil || s.metadata == nil {
		return nil, errors.New("backupapp: metadata source has no store")
	}
	tx, err := s.metadata.BeginMetadataSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("backupapp: opening metadata snapshot: %w", err)
	}
	return &metadataSnapshot{tx: tx}, nil
}

type metadataSnapshot struct {
	tx         *store.MetadataSnapshot
	reader     *io.PipeReader
	exportDone chan error
	opened     bool
	closed     bool
}

var _ backup.MetadataSnapshot = (*metadataSnapshot)(nil)

func (s *metadataSnapshot) OpenMetadata(ctx context.Context) (io.ReadCloser, int64, error) {
	if s.closed {
		return nil, 0, errors.New("backupapp: metadata snapshot is closed")
	}
	if s.opened {
		return nil, 0, errors.New("backupapp: metadata stream already opened")
	}
	s.opened = true
	var count byteCounter
	if err := s.tx.Export(ctx, &count); err != nil {
		return nil, 0, fmt.Errorf("backupapp: sizing metadata JSONL: %w", err)
	}
	reader, writer := io.Pipe()
	s.reader = reader
	s.exportDone = make(chan error, 1)
	go func() {
		exportErr := s.tx.Export(ctx, writer)
		closeErr := writer.CloseWithError(exportErr)
		s.exportDone <- errors.Join(exportErr, closeErr)
	}()
	return reader, count.n, nil
}

func (s *metadataSnapshot) ContentInfo(ctx context.Context) (*backup.ContentInfo, error) {
	if s.closed {
		return nil, errors.New("backupapp: metadata snapshot is closed")
	}
	return (&frozenView{tx: s.tx}).ContentInfo(ctx)
}

func (s *metadataSnapshot) Stats(ctx context.Context) (json.RawMessage, error) {
	if s.closed {
		return nil, errors.New("backupapp: metadata snapshot is closed")
	}
	return (&frozenView{tx: s.tx}).Stats(ctx)
}

func (s *metadataSnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var exportErr error
	if s.reader != nil {
		_ = s.reader.Close()
		exportErr = <-s.exportDone
	}
	return errors.Join(exportErr, s.tx.Close())
}

type byteCounter struct{ n int64 }

func (w *byteCounter) Write(p []byte) (int, error) {
	if int64(len(p)) > math.MaxInt64-w.n {
		return 0, errors.New("backupapp: metadata JSONL size exceeds int64")
	}
	w.n += int64(len(p))
	return len(p), nil
}

type metadataRestorer struct{}

var _ backup.MetadataRestorer = metadataRestorer{}

func (metadataRestorer) RestoreMetadata(
	ctx context.Context, format string, metadata io.Reader, targetPath string,
) (resultErr error) {
	if format != MetadataFormat {
		return fmt.Errorf("backupapp: unsupported metadata format %q", format)
	}
	target, err := store.Open(targetPath)
	if err != nil {
		return fmt.Errorf("backupapp: creating restored metadata store: %w", err)
	}
	open := true
	defer func() {
		if open {
			resultErr = errors.Join(resultErr, target.Close())
		}
	}()
	if err := target.ImportMetadata(ctx, metadata); err != nil {
		return fmt.Errorf("backupapp: importing metadata JSONL: %w", err)
	}
	if err := target.Checkpoint(ctx); err != nil {
		return fmt.Errorf("backupapp: checkpointing restored metadata: %w", err)
	}
	closeErr := target.Close()
	open = false
	if closeErr != nil {
		return fmt.Errorf("backupapp: closing restored metadata store: %w", closeErr)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(targetPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backupapp: removing restored metadata sidecar %s: %w", suffix, err)
		}
	}
	return nil
}
