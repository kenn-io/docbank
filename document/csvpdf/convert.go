package csvpdf

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/ocr"
)

// Convert consumes and closes source.Content on every path, without network I/O.
// Cancellation is checked between reads; arbitrary blocking readers cannot be interrupted.
func Convert(ctx context.Context, source ocr.Source, policy Policy) (result *Result, err error) {
	if source.Content != nil {
		defer func() {
			if closeErr := source.Content.Close(); closeErr != nil {
				result = nil
				err = errors.Join(err, errors.New("close CSV source failed"))
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.fingerprint == "" {
		return nil, errors.New("CSV PDF policy is invalid; use NewPolicy")
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if source.MediaType != "text/csv" {
		return nil, errors.New("CSV source requires text/csv")
	}
	if source.Size > policy.limits.MaxSourceBytes {
		return nil, errors.New("CSV source exceeds byte limit")
	}
	content, err := io.ReadAll(io.LimitReader(contextReader{ctx, source.Content}, source.Size+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("read CSV source failed")
	}
	if int64(len(content)) != source.Size || digest(content) != source.SHA256 {
		return nil, errors.New("CSV source does not match declared size and SHA-256")
	}
	if !utf8.Valid(content) {
		return nil, errors.New("CSV source is not UTF-8")
	}
	records, err := parse(ctx, content, policy.limits)
	if err != nil {
		return nil, err
	}
	pdf, spans, pages, err := render(ctx, records, policy.limits)
	if err != nil {
		return nil, err
	}
	count, err := media.CountPDFPages(pdf)
	if err != nil {
		return nil, fmt.Errorf("verify generated PDF: %w", err)
	}
	if count != int64(pages) || count > int64(policy.limits.MaxPages) {
		return nil, errors.New("generated PDF page count does not match layout")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Result{pdf: pdf, receipt: Receipt{SourceSHA256: source.SHA256, SourceBytes: source.Size, PDFSHA256: digest(pdf), PDFBytes: int64(len(pdf)), Pages: pages, PolicyFingerprint: policy.fingerprint, ConverterVersion: ConverterVersion, Spans: spans}}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func parse(ctx context.Context, content []byte, limits Limits) ([][]string, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	var records [][]string
	cells := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("CSV source has invalid record syntax")
		}
		if len(records) == limits.MaxRecords || len(record) > limits.MaxCells-cells {
			return nil, errors.New("CSV source exceeds record or cell limit")
		}
		for _, cell := range record {
			if len(cell) > limits.MaxCellBytes {
				return nil, errors.New("CSV source exceeds cell byte limit")
			}
		}
		cells += len(record)
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, errors.New("CSV source has no records")
	}
	return records, nil
}
