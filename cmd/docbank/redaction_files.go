package main

import (
	"context"
	"errors"
	"io"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

func readRedactionFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	data, err := redactionfiles.ReadFile(ctx, path, limit)
	return data, redactionInputCLIError(err)
}

func readRedactionInputs(ctx context.Context, preparationPath, masksPath string) (processing.RedactionResolveRequest, error) {
	inputs, err := redactionfiles.ReadInputs(ctx, preparationPath, masksPath)
	return inputs, redactionInputCLIError(err)
}

func readRedactionReview(ctx context.Context, path string, request processing.RedactionResolveRequest) (processing.RedactionReview, error) {
	review, err := redactionfiles.ReadReview(ctx, path, request)
	return review, redactionInputCLIError(err)
}

func redactionInputCLIError(err error) error {
	if errors.Is(err, redactionfiles.ErrInvalidInput) {
		return usageError(err)
	}
	return redactionCLIError(err)
}

func redactionCLIError(err error) error {
	if errors.Is(err, pdfproduction.ErrVerifierVersion) {
		return &exitError{code: exitGeneral, err: err}
	}
	if _, ok := errors.AsType[*pdfproduction.RSSLimitError](err); ok {
		return &exitError{code: exitBusy, err: err}
	}
	if errors.Is(err, pdfproduction.ErrPDFIntegrity) {
		return integrityError(err)
	}
	if errors.Is(err, processing.ErrRedactionReviewChanged) {
		return &exitError{code: exitStale, err: err}
	}
	if errors.Is(err, processing.ErrRedactionInvalidRequest) || errors.Is(err, processing.ErrRedactionInvalidPreparation) {
		return usageError(err)
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "invalid_redaction_request", "invalid_redaction_preparation":
		return usageError(err)
	case "source_stale", "redaction_preparation_changed", "redaction_review_changed", "bates_reservation_conflict":
		return &exitError{code: exitStale, err: err}
	case "redaction_busy", "redaction_memory":
		return &exitError{code: exitBusy, err: err}
	case "redaction_integrity":
		return integrityError(err)
	}
	return err
}

func writeRedactionProposal(output io.Writer, err error) error {
	var problem *redaction.Problem
	if errors.As(err, &problem) && problem.Code == "selection_expansion_required" {
		return errors.Join(err, writeCLIJSON(output, problem))
	}
	return err
}
