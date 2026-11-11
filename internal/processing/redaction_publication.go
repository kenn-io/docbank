package processing

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

// PublishNumberedPDF returns the first retained, verified numbered PDF, without
// building a ZIP or extending the lifetime of any temporary preview.
func (s *Service) PublishNumberedPDF(ctx context.Context, request RedactionRenderRequest,
	runtime *pagerender.Runtime,
) (store.BatesArtifact, error) {
	return s.publishNumberedReview(ctx, request, runtime, nativeRedactionControls{limits: defaultNativeRedactionLimits})
}

func (s *Service) publishNumberedReview(ctx context.Context, request RedactionRenderRequest,
	runtime *pagerender.Runtime, controls nativeRedactionControls,
) (result store.BatesArtifact, resultErr error) {
	if request.AllocationID == uuid.Nil() || !canonical.IsSHA256Hex(request.ReviewSHA256) {
		return result, ErrRedactionInvalidRequest
	}
	if err := ValidateRedactionMasks(request.Masks); err != nil {
		return result, err
	}
	if s == nil {
		return result, ErrRedactionPreparationUnavailable
	}
	op, err := s.beginNativePDFOperation(ctx, "publish")
	if err != nil {
		return result, err
	}
	committed := false
	cleanup := func(err error) {
		if committed {
			if err != nil {
				slog.WarnContext(ctx, "numbered PDF published but cleanup failed", "error", err)
			}
		} else {
			resultErr = errors.Join(resultErr, err)
		}
	}
	defer func() { cleanup(op.Close()) }()
	admitted, pdf, err := s.prepareRedactionRender(op.ctx, op, request, runtime, controls)
	if err != nil {
		return result, err
	}
	defer clear(admitted.data)
	defer func() { cleanup(pdf.Close()) }()
	result, err = s.catalog.BatesArtifact(op.ctx, request.AllocationID.String())
	if err != nil {
		return result, err
	}
	if admitted.numbered == nil || result.BlobSHA256 != pdf.digest.SHA256 || result.Size != pdf.digest.Size {
		return result, ErrRedactionPreparationIntegrity
	}
	committed = true
	return result, nil
}
