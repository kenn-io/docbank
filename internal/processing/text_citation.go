package processing

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// ResolveTextCitation returns an exact quotation without retaining its evidence.
func (service *Service) ResolveTextCitation(
	ctx context.Context, citation document.TextCitation,
) (document.ResolvedTextCitation, error) {
	if err := document.ValidateTextCitation(citation); err != nil {
		return document.ResolvedTextCitation{}, err
	}
	var result document.ResolvedTextCitation
	err := service.gate.CaptureContext(ctx, func() error {
		artifact, err := service.catalog.RetainedTextCitation(ctx, citation)
		if err != nil {
			return err
		}
		result, err = readTextCitation(ctx, service.blobs, citation, artifact.Size)
		return err
	})
	return result, err
}

func readTextCitation(
	ctx context.Context, blobs verifiedBlobReader, citation document.TextCitation, size int64,
) (document.ResolvedTextCitation, error) {
	if size > MaxRenditionBytes {
		return document.ResolvedTextCitation{}, document.ErrCitationLimit
	}
	stream, err := openCitationStream(ctx, blobs, citation, size)
	if err != nil {
		return document.ResolvedTextCitation{}, err
	}
	text, readErr := readCitationRange(ctx, io.LimitReader(stream, MaxRenditionBytes+1), citation, size)
	if err := finishCitationStream(ctx, stream, readErr); err != nil {
		return document.ResolvedTextCitation{}, err
	}
	digest := sha256.Sum256([]byte(text))
	return document.ResolvedTextCitation{Citation: citation, Text: text,
		TextSHA256: hex.EncodeToString(digest[:]), TextBytes: len(text)}, nil
}

func openCitationStream(
	ctx context.Context, blobs verifiedBlobReader, citation document.TextCitation, size int64,
) (packstore.VerifiedReadCloser, error) {
	stream, actualSize, err := blobs.OpenStreamContext(ctx, citation.RenditionSHA256)
	if err != nil {
		return nil, citationReadError(err)
	}
	if actualSize != size || size < 0 {
		return nil, citationReadError(errors.Join(document.ErrCitationIntegrity, stream.Close(), ctx.Err()))
	}
	return stream, nil
}

func finishCitationStream(
	ctx context.Context, stream packstore.VerifiedReadCloser, readErr error,
) error {
	// A range refusal is trustworthy only after the full artifact verifies and closes.
	invalidRange := errors.Is(readErr, document.ErrInvalidCitationRange)
	if invalidRange {
		readErr = nil
	}
	if readErr == nil && !stream.Verified() {
		readErr = fmt.Errorf("%w: %w", document.ErrCitationIntegrity, pack.ErrVerificationIncomplete)
	}
	if err := errors.Join(readErr, stream.Close(), ctx.Err()); err != nil {
		return citationReadError(err)
	}
	if invalidRange {
		return document.ErrInvalidCitationRange
	}
	return nil
}

func readCitationRange(
	ctx context.Context, source io.Reader, citation document.TextCitation, size int64,
) (string, error) {
	reader := bufio.NewReaderSize(&contextReader{ctx: ctx, reader: source}, 4096)
	var text strings.Builder
	var bytesRead int64
	codepoints := 0
	for {
		char, width, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading cited rendition: %w", err)
		}
		bytesRead += int64(width)
		if bytesRead > MaxRenditionBytes {
			return "", document.ErrCitationLimit
		}
		if char == utf8.RuneError && width == 1 {
			return "", fmt.Errorf("%w: invalid UTF-8", document.ErrCitationIntegrity)
		}
		if codepoints >= citation.Start && codepoints < citation.End {
			text.WriteRune(char)
		}
		codepoints++
	}
	if bytesRead != size {
		return "", fmt.Errorf("%w: rendition size changed", document.ErrCitationIntegrity)
	}
	if codepoints < citation.End {
		return "", document.ErrInvalidCitationRange
	}
	return text.String(), nil
}

func citationReadError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, packstore.ErrPhysicalMissing):
		return fmt.Errorf("%w: %w", document.ErrCitationUnavailable, err)
	default:
		return CitationIntegrityError(err)
	}
}

// CitationIntegrityError wraps err with document.ErrCitationIntegrity when stored bytes failed verification.
func CitationIntegrityError(err error) error {
	if errors.Is(err, packstore.ErrContentMismatch) || errors.Is(err, packstore.ErrPhysicalCorrupt) ||
		errors.Is(err, pack.ErrChecksum) || errors.Is(err, pack.ErrCorrupt) ||
		errors.Is(err, pack.ErrTruncated) || errors.Is(err, pack.ErrBlobMismatch) {
		return fmt.Errorf("%w: %w", document.ErrCitationIntegrity, err)
	}
	return err
}
