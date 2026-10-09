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
	selected, err := service.selectMediaTranscript(ctx, request)
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

// renditionLookup reads the active rendition for a content version and profile.
type renditionLookup func(
	ctx context.Context, contentVersionID, profileFingerprint string,
) (store.RenditionView, error)

func (service *Service) selectMediaTranscript(
	ctx context.Context, request MediaTranscriptRequest,
) (mediaTranscriptSelection, error) {
	result := MediaTranscript{SourceID: request.SourceID, SourceVersionID: request.SourceVersionID,
		ContentVersionID: request.ContentVersionID, EvidenceState: mediaTranscriptEvidenceUnavailable}
	if service == nil || service.catalog == nil || service.blobs == nil {
		return mediaTranscriptSelection{result: result}, ErrMediaCapabilityUnavailable
	}
	result.VaultUID = service.catalog.VaultID()
	if err := validateMediaTranscriptContentVersion(request.ContentVersionID); err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	item, err := service.catalog.MediaSourceVersion(ctx, service.principal,
		request.SourceID, request.SourceVersionID)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	return service.selectMediaTranscriptItem(ctx, request, item, result,
		service.catalog.ActiveRendition)
}

// selectMediaTranscripts reads selection metadata for many exact source
// versions. Missing sources select no evidence instead of failing the batch.
func (service *Service) selectMediaTranscripts(
	ctx context.Context, requests []MediaTranscriptRequest,
) (map[MediaTranscriptRequest]mediaTranscriptSelection, error) {
	keys := make([]store.MediaSourceVersionKey, len(requests))
	for i, request := range requests {
		keys[i] = store.MediaSourceVersionKey{SourceID: request.SourceID,
			SourceVersionID: request.SourceVersionID}
	}
	items, err := service.catalog.MediaSourceVersions(ctx, service.principal, keys)
	if err != nil {
		return nil, err
	}
	result := make(map[MediaTranscriptRequest]mediaTranscriptSelection, len(requests))
	for i, request := range requests {
		base := MediaTranscript{VaultUID: service.catalog.VaultID(), SourceID: request.SourceID,
			SourceVersionID: request.SourceVersionID, ContentVersionID: request.ContentVersionID,
			EvidenceState: mediaTranscriptEvidenceUnavailable}
		selected := mediaTranscriptSelection{result: base}
		if item, ok := items[keys[i]]; ok {
			selected, err = service.selectMediaTranscriptItem(ctx, request, item, base,
				service.catalog.ActiveRenditionMetadata)
			if errors.Is(err, store.ErrNotFound) {
				selected, err = mediaTranscriptSelection{result: base}, nil
			}
			if err != nil {
				return nil, err
			}
		}
		result[request] = selected
	}
	return result, nil
}

func validateMediaTranscriptContentVersion(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed[6]>>4 != 4 || parsed[8]>>6 != 2 || parsed.String() != id {
		return ErrMediaTranscriptInvalid
	}
	return nil
}

// selectMediaTranscriptItem checks that the source still covers the node's
// current, untrashed version before selecting its rendition.
func (service *Service) selectMediaTranscriptItem(
	ctx context.Context, request MediaTranscriptRequest, item store.MediaSourceProjection,
	result MediaTranscript, lookup renditionLookup,
) (mediaTranscriptSelection, error) {
	processing, coverage, err := service.mediaProcessingAttempts(ctx, item.ProcessingReceipts)
	if err != nil {
		return mediaTranscriptSelection{result: result}, err
	}
	receipt, err := service.mediaSourceReceiptFromAttempts(ctx, item, processing, coverage)
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
	view, err := lookup(ctx, request.ContentVersionID,
		service.mediaTranscriptProfileFromReceipt(coverage))
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
	return service.selectMediaTranscriptInput(ctx, item, result, view)
}

// selectMediaTranscriptInput names the build's origin and hides a supplied
// input that this source version cannot see.
func (service *Service) selectMediaTranscriptInput(
	ctx context.Context, item store.MediaSourceProjection, result MediaTranscript,
	view store.RenditionView,
) (mediaTranscriptSelection, error) {
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
	return mediaTranscriptSelection{result: result, view: view, inputID: inputBinding,
		origin: origin}, nil
}

// mediaTranscriptProfileFromReceipt names the profile of the receipt that
// covers the source, so a pending retry under another profile does not hide it.
func (service *Service) mediaTranscriptProfileFromReceipt(
	receipt *store.MediaPublicationReceipt,
) string {
	if receipt == nil {
		return ""
	}
	if receipt.ProcessingProfileFingerprint != "" {
		return receipt.ProcessingProfileFingerprint
	}
	if profile, ok := service.profiles[receipt.ProcessingProfile]; ok {
		return profile.record.Fingerprint
	}
	return ""
}
