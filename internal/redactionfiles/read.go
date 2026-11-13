// Package redactionfiles reads the private inputs shared by CLI and MCP exports.
package redactionfiles

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
)

var ErrInvalidInput = errors.New("invalid redaction input")

// ReadFile bounds regular-file input before parsing and preserves I/O causes.
func ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, invalid(fmt.Errorf("reading %q: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return nil, invalid(fmt.Errorf("%q must be a regular file", path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalid(fmt.Errorf("reading %q: %w", path, err))
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, invalid(fmt.Errorf("%q must be a regular file of at most %d bytes", path, limit))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, invalid(fmt.Errorf("%q exceeds %d bytes", path, limit))
	}
	return data, nil
}

func ReadInputs(ctx context.Context, preparationPath, masksPath string) (processing.RedactionResolveRequest, error) {
	var result processing.RedactionResolveRequest
	data, err := ReadFile(ctx, preparationPath, 16<<20)
	if err != nil {
		return result, err
	}
	var transport struct {
		processing.RedactionPreparation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return result, invalid(err)
	}
	result.Preparation = transport.RedactionPreparation
	if err := processing.ValidateRedactionPreparation(result.Preparation); err != nil {
		return result, invalid(err)
	}
	if err := production.ValidateNativePreparationDerivation(result.Preparation.NativePreparation); err != nil {
		return result, invalid(err)
	}
	data, err = ReadFile(ctx, masksPath, processing.MaxRedactionMaskBytes)
	if err != nil {
		return result, err
	}
	result.Masks, err = processing.DecodeRedactionMasks(data)
	if err != nil {
		return result, invalid(err)
	}
	return result, ctx.Err()
}

func ReadReview(ctx context.Context, path string, request processing.RedactionResolveRequest) (processing.RedactionReview, error) {
	var transport struct {
		processing.RedactionReview

		Schema string `json:"$schema,omitzero"`
	}
	data, err := ReadFile(ctx, path, 16<<20)
	if err != nil {
		return transport.RedactionReview, err
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return transport.RedactionReview, invalid(err)
	}
	if err := processing.ValidateRedactionReview(request.Preparation, request.Masks, transport.RedactionReview); err != nil {
		return transport.RedactionReview, invalid(err)
	}
	return transport.RedactionReview, ctx.Err()
}

func invalid(err error) error {
	if errors.Is(err, pdfproduction.ErrVerifierVersion) || errors.Is(err, processing.ErrRedactionReviewChanged) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalidInput, err)
}
