package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"time"
	"uuid"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

type publishNumberedPDFInput struct {
	AllocationID    uuid.UUID `json:"allocation_id"`
	ReviewSHA256    string    `json:"review_sha256"`
	PreparationPath string    `json:"preparation_path"`
	MasksPath       string    `json:"masks_path"`
	ReviewPath      string    `json:"review_path"`
}

// Publication errors contain only fixed codes; private reader and daemon causes
// must never reach the detailed local-file failure logger.
type numberedPublicationError struct{ code string }

func (e *numberedPublicationError) Error() string { return e.code }

var numberedPublicationMessages = map[string]string{
	"bates_outcome_unknown":         "The PDF may already be published. Query get_bates_export for this allocation or repeat publish_numbered_pdf with the identical allocation, review digest and files. Do not reserve new numbers.",
	"redaction_busy":                "Another native PDF operation is running in the daemon; retry this allocation when it finishes.",
	"redaction_limit":               "Publication exceeds its size or work limit.",
	"redaction_unsupported":         "The reserved source cannot be processed by the native PDF renderer.",
	"redaction_preparation_changed": "The saved evidence changed; prepare and review the source again.",
	"redaction_review_changed":      "The review does not match these files or this allocation; use the matching saved review.",
	"redaction_verifier_version":    "The preparation, review or reserved recipe requires a different verifier version.",
	"redaction_memory":              "The daemon exceeds the renderer memory ceiling; retry when it is quieter.",
	"redaction_worker_timeout":      "The daemon PDF worker exceeded its per-page deadline.",
	"redaction_canceled":            "Publication was canceled before a possible write.",
	"redaction_timeout":             "Publication exceeded the request deadline before a possible write.",
	"not_found":                     "The selected allocation or retained source is unavailable.",
}

func publishNumberedPDF(ctx context.Context, lease *daemonLease, raw []byte) (batesExportOutput, error) {
	var input publishNumberedPDFInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesExportOutput{}, err
	}
	for _, path := range []string{input.PreparationPath, input.MasksPath, input.ReviewPath} {
		if path == "" || len(path) > maxPathBytes || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return batesExportOutput{}, invalidToolArgumentsError()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request, err := redactionfiles.ReadInputs(ctx, input.PreparationPath, input.MasksPath)
	if err == nil {
		var review processing.RedactionReview
		review, err = redactionfiles.ReadReview(ctx, input.ReviewPath, request)
		if err == nil && review.SHA256 != input.ReviewSHA256 {
			err = processing.ErrRedactionReviewChanged
		}
	}
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, pdfproduction.ErrVerifierVersion) || errors.Is(err, processing.ErrRedactionReviewChanged) {
			return batesExportOutput{}, publicationError(ctx, err)
		}
		return batesExportOutput{}, invalidToolArgumentsError()
	}
	current, err := lease.acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return batesExportOutput{}, publicationError(ctx, err)
		}
		return batesExportOutput{}, daemonAcquisitionError(err)
	}
	receipt, err := current.daemonconn.PublishNumberedPDF(ctx, processing.RedactionRenderRequest{
		AllocationID: input.AllocationID, Preparation: request.Preparation, Masks: request.Masks, ReviewSHA256: input.ReviewSHA256,
	})
	if err != nil {
		if errors.Is(err, daemonconn.ErrNumberedPublicationUnknown) || daemonconn.IsTransportError(err) || ctx.Err() != nil {
			lease.discard(current)
		}
		return batesExportOutput{}, publicationError(ctx, err)
	}
	return batesExportOutput{BatesExport: receipt, privateCache: newPrivateCache()}, nil
}

func publicationError(ctx context.Context, err error) error {
	var code string
	switch {
	case errors.Is(err, daemonconn.ErrNumberedPublicationUnknown):
		code = "bates_outcome_unknown"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = "redaction_timeout"
	case ctx.Err() != nil:
		code = "redaction_canceled"
	case errors.Is(err, processing.ErrRedactionReviewChanged):
		code = "redaction_review_changed"
	case errors.Is(err, pdfproduction.ErrVerifierVersion):
		code = "redaction_verifier_version"
	case errors.Is(err, processing.ErrRedactionPreparationLimit):
		code = "redaction_limit"
	default:
		code, _ = daemonconn.ProblemCode(err)
	}
	if code == "invalid_redaction_request" || code == "invalid_redaction_preparation" {
		return invalidToolArgumentsError()
	}
	if _, known := numberedPublicationMessages[code]; known {
		return &numberedPublicationError{code: code}
	}
	return sanitizedDaemonError(errDaemonRequestFailed, err)
}
