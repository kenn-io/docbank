package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/kit/packstore"
)

// PinnedProductionPDF is returned only by a catalog-authorized source opener.
// Stream bytes remain untrusted until SpoolProductionPDF finishes verification.
type PinnedProductionPDF struct {
	PDFSHA256 string
	Size      int64
	Stream    packstore.VerifiedReadCloser
}

type VerifiedProductionFile struct{ File *os.File }

func (s *VerifiedProductionFile) Close() error {
	if s == nil || s.File == nil {
		return nil
	}
	file := s.File
	s.File = nil
	return errors.Join(file.Close(), os.Remove(file.Name()))
}

type productionContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r productionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// SpoolProductionPDF verifies exact size, SHA-256 and the backing store's
// terminal integrity before PDFium receives an io.ReaderAt. Its private file
// is bounded by the caller's qualified source limit and always removed on error.
func SpoolProductionPDF(ctx context.Context, source PinnedProductionPDF, maxBytes int64) (result *VerifiedProductionFile, resultErr error) {
	if source.Stream != nil {
		defer func() {
			resultErr = errors.Join(resultErr, source.Stream.Close())
			if resultErr != nil && result != nil {
				resultErr = errors.Join(resultErr, result.Close())
				result = nil
			}
		}()
	}
	if ctx == nil || source.Stream == nil || !canonical.IsSHA256Hex(source.PDFSHA256) ||
		source.Size < 1 || source.Size == math.MaxInt64 || maxBytes < 1 || source.Size > maxBytes {
		return nil, ErrJobConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "docbank-production-pdf-*.tmp")
	if err != nil {
		return nil, err
	}
	result = &VerifiedProductionFile{File: file}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(productionContextReader{ctx, source.Stream}, source.Size+1))
	if err != nil {
		return result, err
	}
	if n != source.Size || hex.EncodeToString(hasher.Sum(nil)) != source.PDFSHA256 {
		return result, ErrJobConflict
	}
	if err := source.Stream.Verify(); err != nil || !source.Stream.Verified() {
		return result, errors.Join(ErrJobConflict, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	return result, nil
}
