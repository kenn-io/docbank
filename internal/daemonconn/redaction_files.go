package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

var ErrInvalidRedactionInput = errors.New("invalid redaction input")

// ReadRedactionFile bounds regular-file input before parsing and preserves I/O causes.
func ReadRedactionFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, invalidRedactionInput(fmt.Errorf("reading %q: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return nil, invalidRedactionInput(fmt.Errorf("%q must be a regular file", path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalidRedactionInput(fmt.Errorf("reading %q: %w", path, err))
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, invalidRedactionInput(fmt.Errorf("%q must be a regular file of at most %d bytes", path, limit))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, invalidRedactionInput(fmt.Errorf("%q exceeds %d bytes", path, limit))
	}
	return data, nil
}

func ReadRedactionInputs(ctx context.Context, preparationPath, masksPath string) (processing.RedactionResolveRequest, error) {
	var result processing.RedactionResolveRequest
	data, err := ReadRedactionFile(ctx, preparationPath, 16<<20)
	if err != nil {
		return result, err
	}
	var transport struct {
		processing.RedactionPreparation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return result, invalidRedactionInput(err)
	}
	result.Preparation = transport.RedactionPreparation
	if err := processing.ValidateRedactionPreparation(result.Preparation); err != nil {
		return result, invalidRedactionInput(err)
	}
	if err := pdfproduction.ValidateNativePreparationDerivation(result.Preparation.NativePreparation); err != nil {
		return result, invalidRedactionInput(err)
	}
	data, err = ReadRedactionFile(ctx, masksPath, processing.MaxRedactionMaskBytes)
	if err != nil {
		return result, err
	}
	result.Masks, err = processing.DecodeRedactionMasks(data)
	if err != nil {
		return result, invalidRedactionInput(err)
	}
	return result, ctx.Err()
}

func ReadRedactionReview(ctx context.Context, path string, request processing.RedactionResolveRequest) (processing.RedactionReview, error) {
	var transport struct {
		processing.RedactionReview

		Schema string `json:"$schema,omitzero"`
	}
	data, err := ReadRedactionFile(ctx, path, 16<<20)
	if err != nil {
		return transport.RedactionReview, err
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return transport.RedactionReview, invalidRedactionInput(err)
	}
	if err := processing.ValidateRedactionReview(request.Preparation, request.Masks, transport.RedactionReview); err != nil {
		return transport.RedactionReview, invalidRedactionInput(err)
	}
	return transport.RedactionReview, ctx.Err()
}

func invalidRedactionInput(err error) error {
	if errors.Is(err, pdfproduction.ErrVerifierVersion) || errors.Is(err, processing.ErrRedactionReviewChanged) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalidRedactionInput, err)
}
