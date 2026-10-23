package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

const MaxRedactionPreparationBytes int64 = 15 << 20

var (
	ErrRedactionPreparationInvalid     = errors.New("invalid native PDF selection")
	ErrRedactionPreparationUnavailable = errors.New("native PDF preparation unavailable")
	ErrRedactionPreparationBusy        = errors.New("native PDF preparation already active")
	ErrRedactionPreparationLimit       = errors.New("native PDF preparation exceeds its bounds")
	ErrRedactionPreparationUnsupported = errors.New("native PDF preparation unsupported")
	ErrRedactionPreparationIntegrity   = errors.New("native PDF preparation failed verification")
)

type RedactionPreparationRequest struct {
	Selection store.PageBinding `json:"selection"`
}

type RedactionPreparation struct {
	production.NativePreparation

	Contract  string            `json:"contract"`
	Selection store.PageBinding `json:"selection"`
}

func ValidateRedactionPreparationRequest(request RedactionPreparationRequest) error {
	selection := request.Selection
	if selection.Source.Size > document.MaxPageSourceBytes {
		return ErrRedactionPreparationLimit
	}
	if selection.NodeID < 1 || selection.Revision < 1 {
		return ErrRedactionPreparationInvalid
	}
	if err := selection.Source.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrRedactionPreparationInvalid, err)
	}
	return nil
}

// ValidateRedactionPreparation binds the complete envelope; a bare map is insufficient.
func ValidateRedactionPreparation(result RedactionPreparation) error {
	return validateRedactionPreparation(result, MaxRedactionPreparationBytes)
}

func validateRedactionPreparation(result RedactionPreparation, maxBytes int64) error {
	if result.Contract != "native-pdf-preparation/v1" {
		return ErrRedactionPreparationIntegrity
	}
	if err := ValidateRedactionPreparationRequest(RedactionPreparationRequest{Selection: result.Selection}); err != nil {
		return err
	}
	if result.Selection.Source != result.Evidence.Source {
		return ErrRedactionPreparationIntegrity
	}
	if err := production.ValidateNativePreparation(result.NativePreparation); err != nil {
		return err
	}
	if _, exceeded, err := canonical.BoundedSize(result, maxBytes); err != nil {
		return err
	} else if exceeded {
		return ErrRedactionPreparationLimit
	}
	return nil
}

func (service *Service) PrepareNativePDF(ctx context.Context, request RedactionPreparationRequest,
	runtime *pagerender.Runtime,
) (RedactionPreparation, error) {
	return service.prepareNativePDF(ctx, request, runtime, MaxRedactionPreparationBytes)
}

func (service *Service) prepareNativePDF(ctx context.Context, request RedactionPreparationRequest,
	runtime *pagerender.Runtime, maxResultBytes int64,
) (result RedactionPreparation, resultErr error) {
	if err := ValidateRedactionPreparationRequest(request); err != nil {
		return result, err
	}
	if service == nil || runtime == nil || !pagerender.SupportedPlatform() {
		return result, ErrRedactionPreparationUnavailable
	}
	op, err := service.beginNativePDFOperation(ctx, "prepare")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, op.Close()) }()
	runCtx := op.ctx
	var data []byte
	err = service.gate.CaptureContext(runCtx, func() error {
		data, err = service.capturePreparationSource(runCtx, request.Selection)
		return err
	})
	if err != nil {
		return result, err
	}
	defer clear(data)
	inspection, err := runtime.Inspect(runCtx, data, request.Selection.Source, "application/pdf")
	if err != nil {
		return result, preparationFailure(err)
	}
	prepared, err := production.PrepareNativePDF(runCtx, production.NativePreparationInput{
		PDF: pdfproduction.Source{StagingDirectory: op.directory, Reader: bytes.NewReader(data), Size: int64(len(data)),
			SHA256: request.Selection.Source.SHA256},
		Inspection: inspection, PageRuntimeFingerprint: runtime.Fingerprint(),
	})
	if err != nil {
		return result, preparationFailure(err)
	}
	result = RedactionPreparation{Contract: "native-pdf-preparation/v1",
		Selection: request.Selection, NativePreparation: prepared}
	if err := validateRedactionPreparation(result, maxResultBytes); err != nil {
		if errors.Is(err, ErrRedactionPreparationLimit) {
			return RedactionPreparation{}, err
		}
		return RedactionPreparation{}, fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	}

	if err := runCtx.Err(); err != nil {
		return RedactionPreparation{}, err
	}
	return result, nil
}

func preparationFailure(err error) error {
	var renderProblem *redaction.Problem
	var alignmentProblem *production.Problem
	switch {
	case errors.Is(err, pagerender.ErrUnavailable):
		return fmt.Errorf("%w: %w", ErrRedactionPreparationUnavailable, err)
	case errors.Is(err, pagerender.ErrInvalidOutput):
		return fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	case errors.Is(err, pagerender.ErrUnsupported):
		return fmt.Errorf("%w: %w", ErrRedactionPreparationUnsupported, err)
	case errors.Is(err, production.ErrNativePreparationLimit),
		errors.As(err, &renderProblem) && renderProblem.Code == "render_limit":
		return fmt.Errorf("%w: %w", ErrRedactionPreparationLimit, err)
	case errors.As(err, &alignmentProblem) && alignmentProblem.Code == "mapping_incomplete":
		return fmt.Errorf("%w: %w", ErrRedactionPreparationUnsupported, err)
	default:
		return err
	}
}
