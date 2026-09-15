package processing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

const mediaPageTokenDomain = "media-page/v1\x00" //nolint:gosec // HMAC domain separator, not a credential.

var ErrMediaCursorInvalid = errors.New("invalid media page cursor")

var ErrMediaFilterUnsupported = errors.New("media profile and variant filters are unsupported")

type MediaListOptions struct {
	Cursor, Profile, Variant, SourceID string
	Limit                              int
}

type MediaSourceRow struct {
	SourceID, SourceVersionID, ContentVersionID, Filename, CaptureLabel string
	Outcome, CoverageState, Excerpt                                     string
}

type MediaSourcePage struct {
	Items      []MediaSourceRow
	Total      int
	NextCursor string
}

type MediaOccurrenceRow struct {
	OccurrenceID, SourceID, SourceVersionID, Ref, Revision string
	Filename, PersonRef, SpeakerLabel                      string
	Message                                                MediaTimestamp
}

type MediaOccurrencePage struct {
	Items      []MediaOccurrenceRow
	Total      int
	NextCursor string
}

type mediaPageClaim struct {
	Version, Principal, Kind, Profile, Variant, SourceID string
	Fence                                                int64
	Offset, Limit                                        int
}

func (service *Service) ListMediaSources(ctx context.Context, options MediaListOptions) (MediaSourcePage, error) {
	offset, fence, err := service.mediaPage(ctx, "sources", options)
	if err != nil {
		return MediaSourcePage{}, err
	}
	items, total, err := service.catalog.MediaSources(ctx, service.principal, offset, options.Limit)
	if err != nil {
		return MediaSourcePage{}, err
	}
	result := MediaSourcePage{Items: make([]MediaSourceRow, 0, len(items)), Total: total}
	for _, item := range items {
		receipt, receiptErr := service.mediaSourceReceipt(ctx, item)
		if receiptErr != nil {
			return MediaSourcePage{}, receiptErr
		}
		capture := ""
		var timestamp MediaTimestamp
		if json.Unmarshal([]byte(item.CaptureJSON), &timestamp, json.RejectUnknownMembers(true)) == nil {
			capture = timestamp.Normalized
			if capture == "" {
				capture = timestamp.Raw
			}
		}
		result.Items = append(result.Items, MediaSourceRow{SourceID: item.SourceID,
			SourceVersionID: item.SourceVersionID, ContentVersionID: item.ContentVersionID,
			Filename: item.Filename, CaptureLabel: capture, Outcome: receipt.Outcome,
			CoverageState: receipt.CoverageState})
	}
	if offset+len(items) < total {
		result.NextCursor, err = service.signMediaPage(mediaPageClaim{Version: "v1",
			Principal: service.principal, Kind: "sources", Profile: options.Profile,
			Variant: options.Variant, Fence: fence, Offset: offset + len(items), Limit: options.Limit})
	}
	return result, err
}

func (service *Service) MediaStatus(ctx context.Context, sourceID string) (MediaReceipt, error) {
	item, err := service.catalog.MediaSource(ctx, service.principal, sourceID)
	if err != nil {
		return MediaReceipt{}, err
	}
	receipt, err := service.mediaSourceReceipt(ctx, item)
	if err != nil {
		return MediaReceipt{}, err
	}
	receipt.SourceID, receipt.SourceVersionID = item.SourceID, item.SourceVersionID
	receipt.ContentVersionID, receipt.OccurrenceID = item.ContentVersionID, item.OccurrenceID
	return receipt, nil
}

func (service *Service) mediaSourceReceipt(
	ctx context.Context, item store.MediaSourceProjection,
) (MediaReceipt, error) {
	receipt := mediaReceiptFromStore(item.Receipt)
	if item.ProcessingReceipt != nil {
		receipt.OperationState = item.ProcessingReceipt.OperationState
		receipt.OperationID = item.ProcessingReceipt.OperationID
		receipt.JobID = item.ProcessingReceipt.JobID
		receipt.SuppliedInputID = item.ProcessingReceipt.SuppliedInputID
	}
	coverage := item.CoverageReceipt
	if coverage == nil {
		coverage = item.ProcessingReceipt
	}
	if coverage == nil {
		return receipt, nil
	}
	receipt.CoverageState = coverage.CoverageState
	if coverage.SuppliedInputID != "" {
		visible, err := service.catalog.MediaInputBindingVisible(ctx, service.principal,
			item.SourceID, coverage.SourceVersionID, coverage.SuppliedInputID)
		if err != nil {
			return MediaReceipt{}, err
		}
		if !visible {
			receipt.CoverageState = "stale"
		}
	}
	return receipt, nil
}

func (service *Service) ListMediaOccurrences(ctx context.Context, options MediaListOptions) (MediaOccurrencePage, error) {
	offset, fence, err := service.mediaPage(ctx, "occurrences", options)
	if err != nil {
		return MediaOccurrencePage{}, err
	}
	items, total, err := service.catalog.MediaOccurrences(ctx, service.principal, options.SourceID, offset, options.Limit)
	if err != nil {
		return MediaOccurrencePage{}, err
	}
	result := MediaOccurrencePage{Items: make([]MediaOccurrenceRow, 0, len(items)), Total: total}
	for _, item := range items {
		var timestamp MediaTimestamp
		if err := json.Unmarshal([]byte(item.MessageJSON), &timestamp, json.RejectUnknownMembers(true)); err != nil {
			return MediaOccurrencePage{}, fmt.Errorf("decoding media occurrence timestamp: %w", err)
		}
		result.Items = append(result.Items, MediaOccurrenceRow{OccurrenceID: item.OccurrenceID,
			SourceID: item.SourceID, SourceVersionID: item.SourceVersionID, Ref: item.Ref,
			Revision: item.Revision, Filename: item.Filename, PersonRef: item.PersonRef,
			SpeakerLabel: item.SpeakerLabel, Message: timestamp})
	}
	if offset+len(items) < total {
		result.NextCursor, err = service.signMediaPage(mediaPageClaim{Version: "v1",
			Principal: service.principal, Kind: "occurrences", SourceID: options.SourceID,
			Fence: fence, Offset: offset + len(items), Limit: options.Limit})
	}
	return result, err
}

func (service *Service) DeclareMediaOccurrence(
	ctx context.Context, operationID, sourceID string, occurrence MediaOccurrenceInput,
) (MediaReceipt, error) {
	message, err := canonical.Marshal(occurrence.Message)
	if err != nil {
		return MediaReceipt{}, err
	}
	identity, err := canonical.Marshal(struct {
		SourceID   string
		Occurrence MediaOccurrenceInput
	}{sourceID, occurrence})
	if err != nil {
		return MediaReceipt{}, err
	}
	digest := sha256.Sum256(identity)
	op := store.MediaOperation{ID: operationID, Principal: service.principal,
		Verb: "declare_occurrence", RequestSHA256: hex.EncodeToString(digest[:]), SourceID: sourceID}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, op); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}
	current, err := service.catalog.MediaSource(ctx, service.principal, sourceID)
	if err != nil {
		return MediaReceipt{}, err
	}
	id := mediaOccurrenceID(service.principal, occurrence.Ref, occurrence.Revision)
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		var recordErr error
		stored, recordErr = service.catalog.RecordMediaOccurrence(ctx, op, store.MediaOccurrenceInput{ID: id, SourceID: sourceID,
			SourceVersionID: current.SourceVersionID, Principal: service.principal, Ref: occurrence.Ref,
			Revision: occurrence.Revision, Filename: occurrence.Filename, PersonRef: occurrence.PersonRef,
			SpeakerLabel: occurrence.SpeakerLabel, MessageJSON: string(message)})
		return recordErr
	})
	return mediaReceiptFromStore(stored), err
}

func (service *Service) RevokeMediaOccurrence(
	ctx context.Context, operationID, occurrenceID, expectedRevision string,
) (MediaReceipt, error) {
	digest := sha256.Sum256([]byte(occurrenceID + "\x00" + expectedRevision))
	op := store.MediaOperation{ID: operationID, Principal: service.principal, Verb: "revoke_occurrence",
		RequestSHA256: hex.EncodeToString(digest[:])}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, op); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}
	var stored store.MediaPublicationReceipt
	err := service.mediaMutation(ctx, func() error {
		var recordErr error
		stored, recordErr = service.catalog.RecordMediaOccurrenceRevocation(ctx, op, occurrenceID, expectedRevision)
		return recordErr
	})
	return mediaReceiptFromStore(stored), err
}

func (service *Service) mediaPage(
	ctx context.Context, kind string, options MediaListOptions,
) (int, int64, error) {
	if service == nil {
		return 0, 0, ErrMediaCapabilityUnavailable
	}
	if options.Profile != "" || options.Variant != "" {
		return 0, 0, ErrMediaFilterUnsupported
	}
	if options.Limit < 1 || options.Limit > 250 {
		return 0, 0, errors.New("media page limit must be between 1 and 250")
	}
	fence, err := service.catalog.MediaVisibilityFence(ctx, service.principal)
	if err != nil {
		return 0, 0, err
	}
	if options.Cursor == "" {
		return 0, fence, nil
	}
	claim, err := service.verifyMediaPage(options.Cursor)
	if err != nil || claim.Version != "v1" || claim.Principal != service.principal || claim.Kind != kind ||
		claim.Profile != options.Profile || claim.Variant != options.Variant || claim.SourceID != options.SourceID ||
		claim.Limit != options.Limit || claim.Fence != fence || claim.Offset < 0 {
		return 0, 0, ErrMediaCursorInvalid
	}
	return claim.Offset, fence, nil
}

func (service *Service) signMediaPage(claim mediaPageClaim) (string, error) {
	body, err := canonical.Marshal(claim)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, service.mediaTokenKey[:])
	_, _ = mac.Write([]byte(mediaPageTokenDomain))
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (service *Service) verifyMediaPage(token string) (mediaPageClaim, error) {
	var zero mediaPageClaim
	if len(token) > 4096 {
		return zero, ErrMediaCursorInvalid
	}
	a, b, ok := strings.Cut(token, ".")
	if !ok {
		return zero, ErrMediaCursorInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(body) > 3072 {
		return zero, ErrMediaCursorInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return zero, ErrMediaCursorInvalid
	}
	mac := hmac.New(sha256.New, service.mediaTokenKey[:])
	_, _ = mac.Write([]byte(mediaPageTokenDomain))
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return zero, ErrMediaCursorInvalid
	}
	claim, err := canonical.Decode[mediaPageClaim](body)
	if err != nil {
		return zero, ErrMediaCursorInvalid
	}
	return claim, nil
}

func mediaOccurrenceID(principal, ref, revision string) string {
	h := sha256.New()
	for _, value := range []string{"media-occurrence/v1", principal, ref, revision} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

var _ = slices.Clone([]string(nil))
