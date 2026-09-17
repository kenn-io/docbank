package docbank

import (
	"context"

	internalprocessing "go.kenn.io/docbank/internal/processing"
)

// SubmitSuppliedMedia retains one bounded supplied recording and returns its
// immutable source, version, occurrence, and optional durable job identities.
func (v *Vault) SubmitSuppliedMedia(
	ctx context.Context, request SuppliedMediaRequest,
) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.SubmitSuppliedMedia(ctx, internalprocessing.SuppliedMediaRequest{
		OperationID: request.OperationID, Content: request.Content, Filename: request.Filename,
		MediaType: request.MediaType, SHA256: request.SHA256, ByteLength: request.ByteLength,
		ExistingContentVersionID: request.ExistingContentVersionID,
		Occurrence:               toInternalMediaOccurrence(request.Occurrence),
		Processing:               toInternalMediaProcessing(request.Processing),
	})
	if err != nil {
		return MediaReceipt{}, err
	}
	return fromInternalMediaReceipt(receipt), nil
}

func (v *Vault) SubmitRemoteRecording(ctx context.Context, request RemoteRecordingRequest) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.SubmitRemoteRecording(ctx, internalprocessing.RemoteRecordingRequest{
		OperationID: request.OperationID, ReferenceURL: request.ReferenceURL, CanonicalURL: request.CanonicalURL,
		ProviderHint: request.ProviderHint, CredentialBinding: request.CredentialBinding,
		Acquire: request.Acquire, Occurrence: toInternalMediaOccurrence(request.Occurrence),
		Processing: toInternalMediaProcessing(request.Processing),
	})
	return fromInternalMediaReceipt(receipt), err
}

func (v *Vault) ImportRecordingArtifact(ctx context.Context, request MediaArtifactRequest) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.ImportRecordingArtifact(ctx, internalprocessing.MediaArtifactRequest{
		OperationID: request.OperationID, SourceID: request.SourceID, OccurrenceID: request.OccurrenceID,
		Kind: request.Kind, Origin: request.Origin, Provider: request.Provider, Language: request.Language,
		Filename: request.Filename, MediaType: request.MediaType, SHA256: request.SHA256,
		ByteLength: request.ByteLength, Content: request.Content,
	})
	if err != nil {
		return MediaReceipt{}, err
	}
	return fromInternalMediaReceipt(receipt), nil
}

func (v *Vault) MediaStatus(ctx context.Context, sourceID string) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.MediaStatus(ctx, sourceID)
	return fromInternalMediaReceipt(receipt), err
}

func (v *Vault) RetryMedia(
	ctx context.Context, operationID, sourceID string, request MediaProcessingRequest,
) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.RetryMedia(ctx, operationID, sourceID,
		internalprocessing.MediaProcessingRequest{Profile: request.Profile, SuppliedInputID: request.SuppliedInputID})
	return fromInternalMediaReceipt(receipt), err
}

func (v *Vault) ListMediaSources(ctx context.Context, options MediaListOptions) (MediaSourcePage, error) {
	if err := v.begin(); err != nil {
		return MediaSourcePage{}, err
	}
	defer v.lifecycle.RUnlock()
	page, err := v.processing.ListMediaSources(ctx, internalprocessing.MediaListOptions{
		Cursor:   options.Cursor,
		SourceID: options.SourceID, Limit: options.Limit})
	if err != nil {
		return MediaSourcePage{}, err
	}
	result := MediaSourcePage{Items: make([]MediaSourceRow, len(page.Items)), Total: page.Total, NextCursor: page.NextCursor}
	for index, item := range page.Items {
		result.Items[index] = MediaSourceRow(item)
	}
	return result, nil
}

func (v *Vault) ListMediaOccurrences(ctx context.Context, options MediaListOptions) (MediaOccurrencePage, error) {
	if err := v.begin(); err != nil {
		return MediaOccurrencePage{}, err
	}
	defer v.lifecycle.RUnlock()
	page, err := v.processing.ListMediaOccurrences(ctx, internalprocessing.MediaListOptions{
		Cursor: options.Cursor, SourceID: options.SourceID, Limit: options.Limit})
	if err != nil {
		return MediaOccurrencePage{}, err
	}
	result := MediaOccurrencePage{Items: make([]MediaOccurrenceRow, len(page.Items)), Total: page.Total, NextCursor: page.NextCursor}
	for index, item := range page.Items {
		result.Items[index] = MediaOccurrenceRow{OccurrenceID: item.OccurrenceID, SourceID: item.SourceID,
			SourceVersionID: item.SourceVersionID, Ref: item.Ref, Revision: item.Revision,
			Filename: item.Filename, PersonRef: item.PersonRef, SpeakerLabel: item.SpeakerLabel,
			Message: MediaTimestamp(item.Message)}
	}
	return result, nil
}

func (v *Vault) DeclareMediaOccurrence(
	ctx context.Context, operationID, sourceID string, occurrence MediaOccurrenceInput,
) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.DeclareMediaOccurrence(ctx, operationID, sourceID,
		toInternalMediaOccurrence(occurrence))
	return fromInternalMediaReceipt(receipt), err
}

func (v *Vault) RevokeMediaOccurrence(
	ctx context.Context, operationID, occurrenceID, expectedRevision string,
) (MediaReceipt, error) {
	if err := v.begin(); err != nil {
		return MediaReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	receipt, err := v.processing.RevokeMediaOccurrence(ctx, operationID, occurrenceID, expectedRevision)
	return fromInternalMediaReceipt(receipt), err
}

func toInternalMediaTimestamp(value MediaTimestamp) internalprocessing.MediaTimestamp {
	return internalprocessing.MediaTimestamp{
		Normalized: value.Normalized, Raw: value.Raw, Precision: value.Precision,
		Timezone: value.Timezone, ZoneText: value.ZoneText,
		OffsetSeconds: value.OffsetSeconds, FractionDigits: value.FractionDigits,
	}
}

func toInternalMediaOccurrence(value MediaOccurrenceInput) internalprocessing.MediaOccurrenceInput {
	return internalprocessing.MediaOccurrenceInput{
		Ref: value.Ref, Revision: value.Revision, Filename: value.Filename,
		PersonRef: value.PersonRef, SpeakerLabel: value.SpeakerLabel,
		Message: toInternalMediaTimestamp(value.Message),
	}
}

func toInternalMediaProcessing(value *MediaProcessingRequest) *internalprocessing.MediaProcessingRequest {
	if value == nil {
		return nil
	}
	return &internalprocessing.MediaProcessingRequest{
		Profile: value.Profile, SuppliedInputID: value.SuppliedInputID,
	}
}

func fromInternalMediaReceipt(value internalprocessing.MediaReceipt) MediaReceipt {
	return MediaReceipt{
		VaultUID: value.VaultUID, SourceID: value.SourceID,
		SourceVersionID: value.SourceVersionID, ContentVersionID: value.ContentVersionID,
		OccurrenceID: value.OccurrenceID, OperationID: value.OperationID, JobID: value.JobID,
		Outcome: value.Outcome, OperationState: value.OperationState, CoverageState: value.CoverageState,
		SuppliedInputID: value.SuppliedInputID,
	}
}
