package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
)

func readRedactionFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, usageError(fmt.Errorf("reading %q: %w", path, err))
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, usageError(fmt.Errorf("%q must be a regular file of at most %d bytes", path, limit))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, usageError(fmt.Errorf("%q exceeds %d bytes", path, limit))
	}
	return data, nil
}

func readRedactionInputs(preparationPath, masksPath string) (processing.RedactionResolveRequest, error) {
	var result processing.RedactionResolveRequest
	data, err := readRedactionFile(preparationPath, 16<<20)
	if err != nil {
		return result, err
	}
	var transport struct {
		processing.RedactionPreparation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return result, usageError(err)
	}
	result.Preparation = transport.RedactionPreparation
	if err := processing.ValidateRedactionPreparation(result.Preparation); err != nil {
		return result, usageError(err)
	}
	if err := production.ValidateNativePreparationDerivation(result.Preparation.NativePreparation); err != nil {
		return result, usageError(err)
	}
	data, err = readRedactionFile(masksPath, processing.MaxRedactionMaskBytes)
	if err != nil {
		return result, err
	}
	result.Masks, err = processing.DecodeRedactionMasks(data)
	return result, usageError(err)
}

func readRedactionReview(path string, request processing.RedactionResolveRequest) (processing.RedactionReview, error) {
	var transport struct {
		processing.RedactionReview

		Schema string `json:"$schema,omitzero"`
	}
	data, err := readRedactionFile(path, 16<<20)
	if err != nil {
		return transport.RedactionReview, err
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return transport.RedactionReview, usageError(err)
	}
	err = processing.ValidateRedactionReview(request.Preparation, request.Masks, transport.RedactionReview)
	if errors.Is(err, processing.ErrRedactionReviewChanged) {
		return transport.RedactionReview, redactionCLIError(err)
	}
	return transport.RedactionReview, usageError(err)
}

func redactionCLIError(err error) error {
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
	case "source_stale", "redaction_preparation_changed", "redaction_review_changed":
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
