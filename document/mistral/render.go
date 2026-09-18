package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/document/ocr"
	"go.kenn.io/docbank/document/renderpdf"
)

func (p Policy) uploadCandidate(candidate CandidateFormat) CandidateFormat {
	if candidate.ID == formatIDDOCX && p.renderPDF != nil {
		if pdf, ok := CandidateFormatByID(formatIDPDF); ok {
			return pdf
		}
	}
	return candidate
}

func (c *Client) prepareDOCXUpload(
	ctx context.Context, source []byte, snapshot preparedSnapshot,
) ([]byte, preparedSnapshot, renderpdf.Receipt, error) {
	if c.policy.renderPDF == nil {
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, fmt.Errorf(
			"%w: Mistral DOCX rendering is not configured", ErrCapabilityContract,
		)
	}
	original, err := ocr.NewSource(
		io.NopCloser(bytes.NewReader(source)), snapshot.mediaType, snapshot.size, snapshot.sha256,
	)
	if err != nil {
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, fmt.Errorf("%w: %w", ErrInvalidSource, err)
	}
	converted, err := renderpdf.Convert(ctx, original, formatIDDOCX, *c.policy.renderPDF)
	if err != nil {
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, classifyDOCXConversionError(ctx, err)
	}
	pdfBytes := converted.PDF()
	receipt := converted.Receipt()
	if receipt.SourceFormat != formatIDDOCX || receipt.OriginalFormat != formatIDDOCX ||
		receipt.SourceSHA256 != snapshot.sha256 || receipt.SourceBytes != snapshot.size {
		clear(pdfBytes)
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, fmt.Errorf(
			"%w: render PDF receipt does not match the original DOCX", ErrCapabilityContract,
		)
	}
	digest := sha256.Sum256(pdfBytes)
	if receipt.PDFBytes != int64(len(pdfBytes)) || receipt.PDFSHA256 != hex.EncodeToString(digest[:]) ||
		receipt.PDFBytes <= 0 || receipt.Pages <= 0 || receipt.Pages > c.policy.values.MaxUnits {
		clear(pdfBytes)
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, fmt.Errorf(
			"%w: generated PDF does not satisfy the Mistral unit contract", ErrCapabilityContract,
		)
	}
	if receipt.PDFBytes > c.policy.values.MaxDocumentBytes {
		clear(pdfBytes)
		return nil, preparedSnapshot{}, renderpdf.Receipt{}, fmt.Errorf(
			"%w: generated PDF is %d bytes, policy limit %d", ErrCapabilityContract,
			receipt.PDFBytes, c.policy.values.MaxDocumentBytes,
		)
	}
	return pdfBytes, preparedSnapshot{
		format: CandidateFormat{ID: formatIDPDF, Family: "pdf", MediaType: mediaTypePDF, UnitKind: "page"},
		size:   int64(len(pdfBytes)), sha256: receipt.PDFSHA256, mediaType: mediaTypePDF,
		localUnits: receipt.Pages,
	}, receipt, nil
}

func classifyDOCXConversionError(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %w", ErrTransientResponse, err)
	}
	if errors.Is(err, renderpdf.ErrXMLLimit) {
		return fmt.Errorf("%w: %w", ErrCapabilityContract, err)
	}
	if errors.Is(err, renderpdf.ErrRendererChanged) {
		return fmt.Errorf("%w: %w", ErrTransientResponse, err)
	}
	if isDOCXRendererRuntimeError(err) {
		return fmt.Errorf("%w: %w", ErrTransientResponse, err)
	}
	if errors.Is(err, renderpdf.ErrSourceRejected) {
		return fmt.Errorf("%w: %w", ErrInvalidSource, err)
	}
	return fmt.Errorf("%w: %w", ErrCapabilityContract, err)
}

func isDOCXRendererRuntimeError(err error) bool {
	for _, marker := range []error{
		renderpdf.ErrUnavailable,
		renderpdf.ErrPrivateRootUnavailable,
		renderpdf.ErrRuntimeSpecialFile,
		renderpdf.ErrRuntimeIdentityMismatch,
		renderpdf.ErrCanceledBeforeLaunch,
		renderpdf.ErrChildFailed,
	} {
		if errors.Is(err, marker) {
			return true
		}
	}
	return false
}

func readDOCXUpload(ctx context.Context, pdf []byte, snapshot preparedSnapshot) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot.format.ID != formatIDPDF || snapshot.mediaType != mediaTypePDF || snapshot.size <= 0 ||
		int64(len(pdf)) != snapshot.size {
		return nil, fmt.Errorf("%w: generated PDF snapshot is invalid: format=%q media=%q size=%d bytes=%d", ErrCapabilityContract,
			snapshot.format.ID, snapshot.mediaType, snapshot.size, len(pdf))
	}
	pdfCopy := bytes.Clone(pdf)
	digest := sha256.Sum256(pdfCopy)
	if hex.EncodeToString(digest[:]) != snapshot.sha256 {
		clear(pdfCopy)
		return nil, fmt.Errorf("%w: generated PDF changed before upload", ErrCapabilityContract)
	}
	return pdfCopy, nil
}
