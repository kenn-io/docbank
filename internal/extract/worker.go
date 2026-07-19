// Package extract runs bounded, daemon-owned document text extraction.
package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/store"
)

const (
	// TextExtractorName and TextExtractorVersion identify the persisted cache.
	// A behavior change increments the version and naturally queues old rows.
	TextExtractorName    = "plain-text"
	TextExtractorVersion = int64(1)

	// MaxTextBytes bounds both memory and SQLite/FTS growth for one document.
	MaxTextBytes    = int64(16 << 20)
	batchSize       = 64
	defaultInterval = 2 * time.Second
)

type catalog interface {
	SeedTextExtractionQueue(ctx context.Context, extractor string, version int64) error
	PendingTextExtractions(
		ctx context.Context, extractor string, version int64, limit int,
	) ([]store.ExtractionCandidate, error)
	RecordExtraction(ctx context.Context, result store.ExtractionResult) error
}

type blobReader interface {
	OpenStreamContext(
		ctx context.Context, hash string,
	) (packstore.VerifiedReadCloser, int64, error)
}

// Worker discovers text blobs, verifies each complete stream, and writes only
// derived cache rows. mutate shares the daemon's mutation/maintenance gate so
// GC cannot retire a candidate between opening it and recording its result.
type Worker struct {
	catalog  catalog
	blobs    blobReader
	mutate   func(func() error) error
	interval time.Duration
}

func New(catalog catalog, blobs blobReader, mutate func(func() error) error) (*Worker, error) {
	if catalog == nil || blobs == nil {
		return nil, errors.New("text extractor requires catalog and blob reader")
	}
	if mutate == nil {
		mutate = func(fn func() error) error { return fn() }
	}
	return &Worker{catalog: catalog, blobs: blobs, mutate: mutate, interval: defaultInterval}, nil
}

// Run performs an immediate scan, drains all current work in bounded batches,
// and then watches for newly admitted versions until the daemon stops.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.catalog.SeedTextExtractionQueue(
		ctx, TextExtractorName, TextExtractorVersion,
	); err != nil {
		return err
	}
	for {
		processed, err := w.ScanOnce(ctx)
		if err != nil {
			return err
		}
		if processed == batchSize {
			continue
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

// ScanOnce processes one bounded batch and returns the number attempted.
func (w *Worker) ScanOnce(ctx context.Context) (int, error) {
	items, err := w.catalog.PendingTextExtractions(
		ctx, TextExtractorName, TextExtractorVersion, batchSize,
	)
	if err != nil {
		return 0, err
	}
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if err := w.mutate(func() error { return w.extractOne(ctx, item) }); err != nil && !errors.Is(err, store.ErrNotFound) {
			return i, fmt.Errorf("extracting blob %s: %w", item.BlobHash, err)
		}
	}
	return len(items), nil
}

func (w *Worker) extractOne(ctx context.Context, item store.ExtractionCandidate) error {
	if item.Size > MaxTextBytes {
		return w.recordFailure(ctx, item.BlobHash,
			fmt.Sprintf("text exceeds the %d-byte extraction limit", MaxTextBytes))
	}
	stream, streamSize, err := w.blobs.OpenStreamContext(ctx, item.BlobHash)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return w.recordFailure(ctx, item.BlobHash, err.Error())
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, MaxTextBytes+1))
	closeErr := stream.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if int64(len(data)) > MaxTextBytes {
		return w.recordFailure(ctx, item.BlobHash,
			fmt.Sprintf("text exceeds the %d-byte extraction limit", MaxTextBytes))
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return w.recordFailure(ctx, item.BlobHash, err.Error())
	}
	if streamSize != item.Size || int64(len(data)) != item.Size {
		return w.recordFailure(ctx, item.BlobHash, fmt.Sprintf(
			"catalog size %d, stream size %d, read %d", item.Size, streamSize, len(data)))
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return w.recordFailure(ctx, item.BlobHash, "content is not UTF-8 plain text")
	}
	return w.catalog.RecordExtraction(ctx, store.ExtractionResult{
		BlobHash: item.BlobHash, Extractor: TextExtractorName,
		ExtractorVersion: TextExtractorVersion, Status: store.ExtractionOK, Text: string(data),
	})
}

func (w *Worker) recordFailure(ctx context.Context, blobHash, message string) error {
	message = strings.ToValidUTF8(message, "\uFFFD")
	const maxErrorBytes = 1024
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return w.catalog.RecordExtraction(ctx, store.ExtractionResult{
		BlobHash: blobHash, Extractor: TextExtractorName,
		ExtractorVersion: TextExtractorVersion, Status: store.ExtractionFailed, Error: message,
	})
}
