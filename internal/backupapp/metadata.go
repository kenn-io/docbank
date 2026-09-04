package backupapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"strconv"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

// MetadataFormat is the stable Kit manifest identifier for Docbank's
// deterministic logical metadata stream.
const MetadataFormat = "docbank-metadata-jsonl-v1"

const derivativeAuthorityVersion = 1

// DerivativeAuthorityStats describes the catalog-authorized derivative
// authority pinned by one backup snapshot. Future derivative classes extend
// this versioned list; absent classes do not claim unavailable coverage.
type DerivativeAuthorityStats struct {
	Version           int                    `json:"version"`
	Classes           []DerivativeClassStats `json:"classes"`
	ProviderDependent []string               `json:"provider_dependent"`
}

// DerivativeClassStats records one current derivative class. Checksum is a
// deterministic digest of the class's catalog records, not provider output.
type DerivativeClassStats struct {
	Class          string `json:"class"`
	Classification string `json:"classification"`
	Count          int64  `json:"count"`
	LogicalBytes   int64  `json:"logical_bytes"`
	BlobCount      int64  `json:"blob_count"`
	Checksum       string `json:"checksum"`
}

type derivativeClassAccumulator struct {
	DerivativeClassStats

	checksum hash.Hash
	blobs    map[string]struct{}
}

func computeDerivativeAuthorityStats(ctx context.Context, q rowQuerier) (*DerivativeAuthorityStats, bool, error) {
	classes := make(map[string]*derivativeClassAccumulator)
	get := func(classification, class string) *derivativeClassAccumulator {
		item := classes[class]
		if item != nil {
			return item
		}
		item = &derivativeClassAccumulator{
			Class: class, Classification: classification,
			checksum: sha256.New(),
			blobs:    make(map[string]struct{}),
		}
		writeDerivativeClassField(item.checksum, class)
		writeDerivativeClassField(item.checksum, classification)
		classes[class] = item
		return item
	}

	if err := func() error {
		rows, err := q.QueryContext(ctx, `
			SELECT role,build_id,artifact_id,blob_hash,size,checksum
			FROM rendition_artifacts
			ORDER BY role,build_id,artifact_id`)
		if err != nil {
			return fmt.Errorf("listing derivative artifacts: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var role, buildID, artifactID, blobHash, checksum string
			var size int64
			if err := rows.Scan(&role, &buildID, &artifactID, &blobHash, &size, &checksum); err != nil {
				return fmt.Errorf("scanning derivative artifact: %w", err)
			}
			item := get("included", role)
			if err := addDerivativeClassBytes(&item.LogicalBytes, size); err != nil {
				return fmt.Errorf("derivative class %s: %w", role, err)
			}
			item.Count++
			item.blobs[blobHash] = struct{}{}
			for _, field := range []string{buildID, artifactID, blobHash, checksum, strconv.FormatInt(size, 10)} {
				writeDerivativeClassField(item.checksum, field)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating derivative artifacts: %w", err)
		}
		return nil
	}(); err != nil {
		return nil, false, fmt.Errorf("backupapp: %w", err)
	}

	if err := func() error {
		var present bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master
			WHERE type='table' AND name='visual_preview_generations')`).Scan(&present); err != nil {
			return fmt.Errorf("detecting visual preview catalog: %w", err)
		}
		if !present {
			return nil
		}
		rows, err := q.QueryContext(ctx, `
			SELECT generation_id,output_blob_hash,output_size,checksum
			FROM visual_preview_generations
			WHERE state='ready'
			ORDER BY generation_id`)
		if err != nil {
			return fmt.Errorf("listing visual previews: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var generationID, blobHash, checksum string
			var size int64
			if err := rows.Scan(&generationID, &blobHash, &size, &checksum); err != nil {
				return fmt.Errorf("scanning visual preview: %w", err)
			}
			item := get("included", "visual_preview")
			if err := addDerivativeClassBytes(&item.LogicalBytes, size); err != nil {
				return fmt.Errorf("visual preview: %w", err)
			}
			item.Count++
			item.blobs[blobHash] = struct{}{}
			for _, field := range []string{generationID, blobHash, checksum, strconv.FormatInt(size, 10)} {
				writeDerivativeClassField(item.checksum, field)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating visual previews: %w", err)
		}
		return nil
	}(); err != nil {
		return nil, false, fmt.Errorf("backupapp: %w", err)
	}

	if err := func() error {
		rows, err := q.QueryContext(ctx, `
			SELECT build_id,segment_id,checksum,text
			FROM rendition_lexical_segments
			ORDER BY build_id,segment_order,segment_id`)
		if err != nil {
			return fmt.Errorf("listing lexical projection: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var buildID, segmentID, checksum, text string
			if err := rows.Scan(&buildID, &segmentID, &checksum, &text); err != nil {
				return fmt.Errorf("scanning lexical projection: %w", err)
			}
			item := get("reconstructible", "lexical_projection")
			if err := addDerivativeClassBytes(&item.LogicalBytes, int64(len(text))); err != nil {
				return fmt.Errorf("lexical projection: %w", err)
			}
			item.Count++
			for _, field := range []string{buildID, segmentID, checksum, text} {
				writeDerivativeClassField(item.checksum, field)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating lexical projection: %w", err)
		}
		return nil
	}(); err != nil {
		return nil, false, fmt.Errorf("backupapp: %w", err)
	}

	if len(classes) == 0 {
		return nil, false, nil
	}
	// Synthetic classes are local to backupapp and follow persisted roles.
	ordered := append(store.PersistedRenditionArtifactRoles(), "visual_preview", "lexical_projection")
	result := &DerivativeAuthorityStats{
		Version: derivativeAuthorityVersion, ProviderDependent: []string{},
	}
	for _, class := range ordered {
		item := classes[class]
		if item == nil {
			continue
		}
		item.BlobCount = int64(len(item.blobs))
		item.Checksum = hex.EncodeToString(item.checksum.Sum(nil))
		result.Classes = append(result.Classes, item.DerivativeClassStats)
		delete(classes, class)
	}
	if len(classes) != 0 {
		return nil, false, errors.New("backupapp: derivative artifact class is not catalog-authorized")
	}
	return result, true, nil
}

func addDerivativeClassBytes(total *int64, value int64) error {
	if value < 0 || value > math.MaxInt64-*total {
		return errors.New("logical bytes exceed int64")
	}
	*total += value
	return nil
}

func writeDerivativeClassField(dst hash.Hash, value string) {
	_, _ = io.WriteString(dst, strconv.Itoa(len(value)))
	_, _ = io.WriteString(dst, ":")
	_, _ = io.WriteString(dst, value)
}

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
	if err := s.tx.ExportBackup(ctx, &count); err != nil {
		return nil, 0, fmt.Errorf("backupapp: sizing metadata JSONL: %w", err)
	}
	reader, writer := io.Pipe()
	s.reader = reader
	s.exportDone = make(chan error, 1)
	go func() {
		exportErr := s.tx.ExportBackup(ctx, writer)
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

func (s *metadataSnapshot) Stats(ctx context.Context) (jsontext.Value, error) {
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

type metadataRestorer struct{ driver docsqlite.Driver }

var _ backup.MetadataRestorer = metadataRestorer{}

func (r metadataRestorer) RestoreMetadata(
	ctx context.Context, format string, metadata io.Reader, targetPath string,
) (resultErr error) {
	if format != MetadataFormat {
		return fmt.Errorf("backupapp: unsupported metadata format %q", format)
	}
	target, err := store.Open(targetPath, r.driver)
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
