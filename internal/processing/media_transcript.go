package processing

import (
	"context"
	"errors"

	"uuid"

	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

const (
	mediaTranscriptEvidenceReady       = "ready"
	mediaTranscriptEvidencePending     = "pending"
	mediaTranscriptEvidenceUnavailable = "unavailable"
	mediaTranscriptEvidenceStale       = "stale"
)

var ErrMediaTranscriptInvalid = errors.New("media transcript request is invalid")

// MediaTranscriptRequest names one immutable media tuple. The expected
// content version is part of the request so a caller cannot accept a newer
// source revision by accident.
type MediaTranscriptRequest struct {
	SourceID, SourceVersionID, ContentVersionID string
}

type MediaTranscriptUnit struct {
	Text     string
	TimeSpan *retrieval.MediaTimeSpan
	Speaker  string
}

type MediaTranscriptEvidence struct {
	BuildID         string
	SuppliedInputID string
	Origin          string
	Completeness    string
	Truncated       bool
	HasOmissions    bool
	Units           []MediaTranscriptUnit
}

// MediaTranscript is the result of an exact retained-evidence read.
// Transcript is nil unless EvidenceState is ready.
type MediaTranscript struct {
	VaultUID         string
	SourceID         string
	SourceVersionID  string
	ContentVersionID string
	EvidenceState    string
	CoverageState    string
	OperationState   string
	Transcript       *MediaTranscriptEvidence
}

// MediaTranscript returns the normalized evidence of the active rendition for
// one exact source version, through the same lookup as rendition selection.
func (service *Service) MediaTranscript(
	ctx context.Context, request MediaTranscriptRequest,
) (MediaTranscript, error) {
	selected, err := service.selectMediaTranscript(ctx, request, false)
	result := selected.result
	if err != nil || result.EvidenceState != mediaTranscriptEvidenceReady {
		return result, err
	}
	view, origin := selected.view, selected.origin
	evidence, _, err := service.renditionEvidence(ctx, view.Build)
	if err != nil {
		return result, err
	}
	if evidence.Checksum != view.Build.EvidenceChecksum {
		return result, errors.New("normalized evidence disagrees with its build checksum")
	}
	transcript := MediaTranscriptEvidence{BuildID: view.Build.ID, SuppliedInputID: selected.inputID,
		Origin: origin, Completeness: string(view.Build.Completeness),
		Truncated: view.Build.Truncated, HasOmissions: len(evidence.Omissions) > 0,
		Units: make([]MediaTranscriptUnit, len(evidence.Units))}
	for i, unit := range evidence.Units {
		span, err := retrieval.MediaTimeSpanFromLocator(unit.Locator)
		if err != nil {
			return result, err
		}
		transcript.Units[i] = MediaTranscriptUnit{Text: unit.Text, TimeSpan: span, Speaker: unit.Speaker}
		transcript.HasOmissions = transcript.HasOmissions || len(unit.Omissions) > 0
	}
	result.EvidenceState, result.Transcript = mediaTranscriptEvidenceReady, &transcript
	return result, nil
}

type mediaTranscriptSelection struct {
	result          MediaTranscript
	view            store.RenditionView
	inputID, origin string
}

func (service *Service) selectMediaTranscript(ctx context.Context, request MediaTranscriptRequest, metadataOnly bool) (mediaTranscriptSelection, error) {
	result := MediaTranscript{SourceID: request.SourceID, SourceVersionID: request.SourceVersionID,
		ContentVersionID: request.ContentVersionID, EvidenceState: mediaTranscriptEvidenceUnavailable}
	if service == nil || service.catalog == nil || service.blobs == nil {
		return mediaTranscriptSelection{result: result}, ErrMediaCapabilityUnavailable
	}
	result.VaultUID = service.catalog.VaultID()
	parsed, err := uuid.Parse(request.ContentVersionID)
	if err != nil || (parsed[6]>>4 != 4 || parsed[8]>>6 != 2) || parsed.String() != request.ContentVersionID {
		return mediaTranscriptSelection{result: result}, ErrMediaTranscriptInvalid
	}
	item, err := service.catalog.MediaSourceVersion(ctx, service.principal, request.SourceID, request.SourceVersionID)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	receipt, err := service.mediaSourceReceipt(ctx, item)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	result.CoverageState, result.OperationState = receipt.CoverageState, receipt.OperationState
	if item.ContentVersionID != request.ContentVersionID || receipt.CoverageState == "stale" {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return mediaTranscriptSelection{result: result}, nil
	}
	version, err := service.catalog.ContentVersionByID(ctx, request.ContentVersionID)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	node, err := service.catalog.NodeByID(ctx, version.NodeID)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	// Rendition selection serves only the node's current, untrashed version.
	if node.TrashedAt != nil {
		return mediaTranscriptSelection{result: result}, nil
	}
	if node.CurrentVersionID != version.ID {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return mediaTranscriptSelection{result: result}, nil
	}
	profile, err := service.mediaTranscriptProfile(ctx, item)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	lookup := service.catalog.ActiveRendition
	if metadataOnly {
		lookup = service.catalog.ActiveRenditionMetadata
	}
	view, err := lookup(ctx, request.ContentVersionID, profile)
	if errors.Is(err, store.ErrNotFound) {
		if receipt.CoverageState != "transcribed" &&
			(receipt.OperationState == "queued" || receipt.OperationState == "running") {
			result.EvidenceState = mediaTranscriptEvidencePending
		}
		return mediaTranscriptSelection{result: result}, nil
	}
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	inputBinding, err := service.catalog.RenditionInputBinding(ctx, view.Build.ID)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	origin := "generated"
	if inputBinding != "" {
		// A shared recording's active build may come from another source version's supplied input.
		visible, err := service.catalog.MediaInputBindingVisible(ctx, service.principal,
			item.SourceID, item.SourceVersionID, inputBinding)
		if err != nil {
			return mediaTranscriptSelection{result: result}, err
		}
		if !visible {
			result.EvidenceState = mediaTranscriptEvidenceStale
			return mediaTranscriptSelection{result: result}, nil
		}
		origin = "supplied"
	}
	result.EvidenceState = mediaTranscriptEvidenceReady
	return mediaTranscriptSelection{result: result, view: view, inputID: inputBinding, origin: origin}, nil
}

// mediaTranscriptProfile names the profile of the receipt that covers the
// source, so a pending retry under another profile does not hide it.
func (service *Service) mediaTranscriptProfile(
	ctx context.Context, item store.MediaSourceProjection,
) (string, error) {
	_, receipt, err := service.mediaProcessingAttempts(ctx, item.ProcessingReceipts)
	if err != nil || receipt == nil {
		return "", err
	}
	if receipt.ProcessingProfileFingerprint != "" {
		return receipt.ProcessingProfileFingerprint, nil
	}
	if profile, ok := service.profiles[receipt.ProcessingProfile]; ok {
		return profile.record.Fingerprint, nil
	}
	return "", nil
}
