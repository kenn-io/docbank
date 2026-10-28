package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

type admittedRedaction struct {
	data         []byte
	revision     int64
	review       RedactionReview
	presentation []pdfproduction.PagePresentation
	numbered     *NumberedDelivery
}

func (service *Service) ResolveNativePDF(ctx context.Context, request RedactionResolveRequest,
	runtime *pagerender.Runtime,
) (result RedactionReview, resultErr error) {
	if err := ValidateRedactionMasks(request.Masks); err != nil {
		return result, err
	}
	if service == nil || runtime == nil || !pagerender.SupportedPlatform() {
		return result, ErrRedactionPreparationUnavailable
	}
	op, err := service.beginNativePDFOperation(ctx, "resolve")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, op.Close()) }()
	admitted, err := service.freshRedactionReview(op.ctx, op, request, runtime)
	if err != nil {
		return result, err
	}
	defer clear(admitted.data)
	return admitted.review, nil
}

func (service *Service) freshRedactionReview(ctx context.Context, op *nativePDFOperation,
	request RedactionResolveRequest, runtime *pagerender.Runtime,
) (result admittedRedaction, resultErr error) {
	if err := ValidateRedactionPreparation(request.Preparation); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	if err := production.ValidateNativePreparationDerivation(request.Preparation.NativePreparation); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	selected := NativePDFSelection{NodeID: request.Preparation.Selection.NodeID, Source: request.Preparation.Selection.Source}
	var data []byte
	defer func() {
		if resultErr != nil {
			clear(data)
		}
	}()
	err := service.gate.CaptureContext(ctx, func() error {
		view, err := service.loadNativePDFVersion(ctx, selected)
		if err != nil {
			return err
		}
		result.revision = view.Node.Revision
		data, err = service.readNativePDFSource(ctx, selected, view)
		if errors.Is(err, ErrRedactionPreparationInvalid) {
			return fmt.Errorf("%w: %w", ErrRedactionInvalidRequest, err)
		}
		return err
	})
	if err != nil {
		return result, err
	}
	result.data = data
	return service.reviewCapturedPDF(ctx, op, request, runtime, result)
}

func (service *Service) reviewCapturedPDF(ctx context.Context, op *nativePDFOperation,
	request RedactionResolveRequest, runtime *pagerender.Runtime, result admittedRedaction,
) (admittedRedaction, error) {
	data := result.data
	selected := NativePDFSelection{NodeID: request.Preparation.Selection.NodeID, Source: request.Preparation.Selection.Source}
	inspection, err := runtime.Inspect(ctx, data, selected.Source, "application/pdf")
	if err != nil {
		return result, preparationFailure(err)
	}
	fresh, err := production.PrepareNativePDF(ctx, production.NativePreparationInput{
		PDF:        pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: selected.Source.SHA256, StagingDirectory: op.directory},
		Inspection: inspection, PageRuntimeFingerprint: runtime.Fingerprint()})
	if err != nil {
		return result, preparationFailure(err)
	}
	freshEnvelope := RedactionPreparation{Contract: "native-pdf-preparation/v1", Selection: request.Preparation.Selection, NativePreparation: fresh}
	submittedSHA, err := redactionPreparationDigest(request.Preparation)
	if err != nil {
		return result, err
	}
	freshSHA, err := redactionPreparationDigest(freshEnvelope)
	if err != nil {
		return result, err
	}
	if freshSHA != submittedSHA {
		return result, ErrRedactionPreparationChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.review, err = BuildRedactionReview(freshEnvelope, request.Masks, result.revision)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.data = data
	return result, nil
}
