package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

// MediaPublicationRequest atomically binds core bytes to supplied source,
// occurrence, and replay authority. An empty ContentVersion.ID seals a new
// version at VirtualPath with Physical in the same transaction.
type MediaPublicationRequest struct {
	Operation                                                          MediaOperation
	SourceID                                                           string
	ContentVersion                                                     ContentVersion
	VirtualPath                                                        string
	Physical                                                           BlobPhysical
	Occurrence                                                         MediaOccurrenceInput
	CaptureJSON                                                        string
	ClaimSHA256                                                        string
	ProcessingProfile                                                  string
	SuppliedInputID                                                    string
	ProcessingPrincipal, ProcessingScope, ProcessingProfileFingerprint string
	ProcessingAuthorization                                            ProviderOperationAuthorizationRequest
}

// MediaPublicationReceipt is the sanitized durable result of supplied-media
// retention. It is also the canonical media operation receipt payload.
type MediaPublicationReceipt struct {
	VaultUID                     string                                `json:"vault_uid"`
	SourceID                     string                                `json:"source_id"`
	SourceVersionID              string                                `json:"source_version_id"`
	ContentVersionID             string                                `json:"content_version_id"`
	OccurrenceID                 string                                `json:"occurrence_id"`
	OperationID                  string                                `json:"operation_id"`
	JobID                        string                                `json:"job_id,omitempty"`
	Outcome                      string                                `json:"outcome,omitempty"`
	CoverageState                string                                `json:"coverage_state"`
	OperationState               string                                `json:"operation_state"`
	ProcessingNodeID             int64                                 `json:"processing_node_id,omitempty"`
	ProcessingProfile            string                                `json:"processing_profile,omitempty"`
	SuppliedInputID              string                                `json:"supplied_input_id,omitempty"`
	ProcessingPrincipal          string                                `json:"processing_principal,omitempty"`
	ProcessingScope              string                                `json:"processing_scope,omitempty"`
	ProcessingProfileFingerprint string                                `json:"processing_profile_fingerprint,omitempty"`
	ProcessingAuthorization      ProviderOperationAuthorizationRequest `json:"processing_authorization,omitzero"`
}

// SuppliedTranscriptInput identifies the exact visible retained transcript
// bytes selected for one sealed recording digest.
type SuppliedTranscriptInput struct {
	InputID, OccurrenceID, SourceVersionID, ContentVersionID, InputSHA256 string
	Kind, Origin, Provider, Language                                      string
}

const (
	MediaInputTranscript = "transcript"
	MediaInputCaption    = "caption"
)

func validateSuppliedInputKind(kind string) error {
	if kind != MediaInputTranscript && kind != MediaInputCaption {
		return errors.New("invalid supplied input kind")
	}
	return nil
}

// SuppliedMediaContentVersion returns the already authoritative original for
// a retained supplied source identity, independent of its virtual path.
func (s *Store) SuppliedMediaContentVersion(
	ctx context.Context, sourceID, sourceSHA256 string, sourceBytes int64,
) (ContentVersion, error) {
	var versionID string
	err := s.db.QueryRowContext(ctx, `SELECT v.content_version_id FROM media_source_heads h
		JOIN media_source_versions v ON v.source_version_id=h.source_version_id
		WHERE h.source_id=? AND v.source_sha256=? AND v.source_bytes=?`,
		sourceID, sourceSHA256, sourceBytes).Scan(&versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ContentVersion{}, ErrNotFound
	}
	if err != nil {
		return ContentVersion{}, err
	}
	return s.ContentVersionByID(ctx, versionID)
}

// SuppliedTranscriptForSource selects one caller-authorized transcript input
// bound to the exact supplied recording source version.
func (s *Store) SuppliedTranscriptForSource(
	ctx context.Context, principal, sourceSHA256 string,
) (SuppliedTranscriptInput, error) {
	if err := validateBoundedMediaText("media principal", principal, 256, false); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if !canonical.IsSHA256Hex(sourceSHA256) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	var result SuppliedTranscriptInput
	err := s.db.QueryRowContext(ctx, `SELECT i.input_id,i.occurrence_id,i.source_version_id,
		i.content_version_id,i.input_sha256,i.kind,i.origin,i.provider,i.language
		FROM media_input_artifacts i
		JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
		JOIN media_source_versions v ON v.source_version_id=i.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND i.kind='transcript'
		  AND i.source_id=o.source_id AND v.source_id=o.source_id
		  AND o.source_version_id=v.source_version_id AND v.source_sha256=?
		ORDER BY i.created_at DESC,i.input_id DESC LIMIT 1`, principal, sourceSHA256).Scan(
		&result.InputID, &result.OccurrenceID, &result.SourceVersionID,
		&result.ContentVersionID, &result.InputSHA256, &result.Kind,
		&result.Origin, &result.Provider, &result.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	return result, err
}

// SuppliedTranscriptForSourceID selects a supplied input of the requested kind
// for a known source while
// its source version is still being established. An empty inputID selects the
// newest visible input; a nonempty inputID selects that exact input.
func (s *Store) SuppliedTranscriptForSourceID(
	ctx context.Context, principal, kind, sourceID, sourceSHA256, inputID string,
) (SuppliedTranscriptInput, error) {
	if err := validateBoundedMediaText("media principal", principal, 256, false); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if err := validateSuppliedInputKind(kind); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if err := validateBoundedMediaText("media source", sourceID, 256, false); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if !canonical.IsSHA256Hex(sourceSHA256) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	if inputID != "" && !canonical.IsSHA256Hex(inputID) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	query := `SELECT i.input_id,i.occurrence_id,i.source_version_id,
		i.content_version_id,i.input_sha256,i.kind,i.origin,i.provider,i.language
		FROM media_input_artifacts i
		JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
		JOIN media_source_versions v ON v.source_version_id=i.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND i.kind=?
		  AND i.source_id=? AND i.source_id=o.source_id
		  AND o.source_version_id=v.source_version_id AND v.source_id=?
		  AND v.source_sha256=?`
	args := []any{principal, kind, sourceID, sourceID, sourceSHA256}
	if inputID != "" {
		query += " AND i.input_id=?"
		args = append(args, inputID)
	} else {
		query += " ORDER BY i.created_at DESC,i.input_id DESC LIMIT 1"
	}
	return s.querySuppliedTranscript(ctx, query, args...)
}

// SuppliedTranscriptForSourceVersion selects one caller-authorized supplied input
// bound to one exact source version, even when another source has equal bytes.
// An empty inputID selects the newest visible input; otherwise it must match.
func (s *Store) SuppliedTranscriptForSourceVersion(
	ctx context.Context, principal, kind, sourceID, sourceVersionID, inputID string,
) (SuppliedTranscriptInput, error) {
	if err := validateBoundedMediaText("media principal", principal, 256, false); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if err := validateSuppliedInputKind(kind); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	for name, value := range map[string]string{
		"media source": sourceID, "media source version": sourceVersionID,
	} {
		if err := validateBoundedMediaText(name, value, 256, false); err != nil {
			return SuppliedTranscriptInput{}, err
		}
	}
	if inputID != "" && !canonical.IsSHA256Hex(inputID) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	query := `SELECT i.input_id,i.occurrence_id,i.source_version_id,
		i.content_version_id,i.input_sha256,i.kind,i.origin,i.provider,i.language
		FROM media_input_artifacts i
		JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
		WHERE o.caller_principal=? AND o.visible=1 AND i.kind=?
		  AND i.source_id=? AND i.source_version_id=?
		  AND o.source_id=i.source_id AND o.source_version_id=i.source_version_id`
	args := []any{principal, kind, sourceID, sourceVersionID}
	if inputID != "" {
		query += " AND i.input_id=?"
		args = append(args, inputID)
	} else {
		query += " ORDER BY i.created_at DESC,i.input_id DESC LIMIT 1"
	}
	return s.querySuppliedTranscript(ctx, query, args...)
}

// SuppliedTranscriptBindingForSource resolves one exact retained input of the
// requested kind only
// while its occurrence and source-version authority remain visible.
func (s *Store) SuppliedTranscriptBindingForSource(
	ctx context.Context, principal, kind, sourceSHA256, inputID string,
) (SuppliedTranscriptInput, error) {
	if err := validateBoundedMediaText("media principal", principal, 256, false); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if err := validateSuppliedInputKind(kind); err != nil {
		return SuppliedTranscriptInput{}, err
	}
	if !canonical.IsSHA256Hex(sourceSHA256) || !canonical.IsSHA256Hex(inputID) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	var result SuppliedTranscriptInput
	err := s.db.QueryRowContext(ctx, `SELECT i.input_id,i.occurrence_id,i.source_version_id,
		i.content_version_id,i.input_sha256,i.kind,i.origin,i.provider,i.language
		FROM media_input_artifacts i
		JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
		JOIN media_source_versions v ON v.source_version_id=i.source_version_id
		WHERE i.input_id=? AND o.caller_principal=? AND o.visible=1 AND i.kind=?
		  AND i.source_id=o.source_id AND v.source_id=o.source_id
		  AND o.source_version_id=v.source_version_id AND v.source_sha256=?`,
		inputID, principal, kind, sourceSHA256).Scan(&result.InputID, &result.OccurrenceID,
		&result.SourceVersionID, &result.ContentVersionID, &result.InputSHA256,
		&result.Kind, &result.Origin, &result.Provider, &result.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	return result, err
}

func (s *Store) querySuppliedTranscript(
	ctx context.Context, query string, args ...any,
) (SuppliedTranscriptInput, error) {
	var result SuppliedTranscriptInput
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&result.InputID, &result.OccurrenceID,
		&result.SourceVersionID, &result.ContentVersionID, &result.InputSHA256,
		&result.Kind, &result.Origin, &result.Provider, &result.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return SuppliedTranscriptInput{}, ErrNotFound
	}
	return result, err
}

// MediaInputBindingVisible reports whether one exact selected input remains
// reachable through its original caller-visible occurrence and source version.
func (s *Store) MediaInputBindingVisible(
	ctx context.Context, principal, sourceID, sourceVersionID, inputID string,
) (bool, error) {
	var visible int
	err := s.db.QueryRowContext(ctx, `SELECT o.visible FROM media_input_artifacts i
		JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
		WHERE i.input_id=? AND i.source_id=? AND i.source_version_id=?
			AND o.caller_principal=? AND o.source_version_id=i.source_version_id`,
		inputID, sourceID, sourceVersionID, principal).Scan(&visible)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return visible == 1, err
}

// RetainSuppliedMedia records one successful local retention operation. The
// same source bytes reuse the existing source revision while a distinct
// caller occurrence keeps its own immutable authority.
func (s *Store) RetainSuppliedMedia(
	ctx context.Context, request MediaPublicationRequest,
) (MediaPublicationReceipt, error) {
	if request.Operation.SourceID != request.SourceID || request.Operation.Verb != "submit_supplied_media" {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	if request.Occurrence.SourceID != request.SourceID || request.Occurrence.ID != "" ||
		request.Occurrence.SourceVersionID != "" {
		return MediaPublicationReceipt{}, ErrMediaOccurrenceConflict
	}
	if !canonical.IsSHA256Hex(request.SourceID) || !canonical.IsSHA256Hex(request.ContentVersion.BlobHash) {
		return MediaPublicationReceipt{}, ErrMediaSourceConflict
	}
	if _, err := canonicalJSONText(request.CaptureJSON, "media capture claim"); err != nil ||
		!canonical.IsSHA256Hex(request.ClaimSHA256) {
		return MediaPublicationReceipt{}, ErrMediaSourceConflict
	}
	if request.ProcessingProfile != "" {
		if err := validateBoundedMediaText("media processing profile", request.ProcessingProfile, 128, false); err != nil {
			return MediaPublicationReceipt{}, err
		}
		if !canonical.IsSHA256Hex(request.ProcessingProfileFingerprint) ||
			request.ProcessingPrincipal != request.ProcessingAuthorization.Principal ||
			request.ProcessingScope != request.ProcessingAuthorization.Scope ||
			request.ProcessingProfileFingerprint != request.ProcessingAuthorization.ProfileFingerprint ||
			request.ProcessingAuthorization.PriorAuthorization == nil {
			return MediaPublicationReceipt{}, ErrMediaOperationConflict
		}
		if _, err := normalizeConsentAuthority(request.ProcessingAuthorization); err != nil {
			return MediaPublicationReceipt{}, err
		}
	} else if request.SuppliedInputID != "" || request.ProcessingPrincipal != "" ||
		request.ProcessingScope != "" || request.ProcessingProfileFingerprint != "" ||
		request.ProcessingAuthorization.PriorAuthorization != nil ||
		request.ProcessingAuthorization.Principal != "" {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}

	operation := s.withMediaOperation
	if request.ProcessingProfile != "" {
		operation = s.withQueuedMediaOperation
	}
	receiptJSON, err := operation(ctx, request.Operation, func(tx *sql.Tx) (string, error) {
		if request.ContentVersion.ID == "" {
			version, err := s.sealMediaContentTx(ctx, tx, request.VirtualPath, request.ContentVersion, request.Physical)
			if err != nil {
				return "", err
			}
			request.ContentVersion = version
		}
		if err := ensureSuppliedMediaSourceTx(ctx, tx, request.SourceID); err != nil {
			return "", err
		}
		var sourceVersionID string
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT h.source_version_id,h.revision
			FROM media_source_heads h JOIN media_source_versions v
			ON v.source_version_id=h.source_version_id
			WHERE h.source_id=? AND v.content_version_id=? AND v.source_sha256=? AND v.source_bytes=?`,
			request.SourceID, request.ContentVersion.ID, request.ContentVersion.BlobHash,
			request.ContentVersion.Size).Scan(&sourceVersionID, &revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if scanErr := tx.QueryRowContext(ctx, `SELECT revision FROM media_source_heads WHERE source_id=?`,
				request.SourceID).Scan(&revision); errors.Is(scanErr, sql.ErrNoRows) {
				revision = 0
			} else if scanErr != nil {
				return "", scanErr
			}
			if revision != 0 {
				return "", ErrMediaSourceConflict
			}
			sourceVersionID, err = newUUIDv4()
			if err != nil {
				return "", err
			}
		}
		storedOccurrence, found, err := mediaOccurrenceByRevisionTx(ctx, tx,
			request.Occurrence.Principal, request.Occurrence.Ref, request.Occurrence.Revision)
		if err != nil {
			return "", err
		}
		occurrenceID := storedOccurrence.ID
		if !found {
			occurrenceID, err = newUUIDv4()
			if err != nil {
				return "", err
			}
		}
		occurrence := request.Occurrence
		occurrence.ID = occurrenceID
		if revision != 0 {
			occurrence.SourceVersionID = sourceVersionID
		}
		if err := s.declareMediaOccurrenceTx(ctx, tx, occurrence); err != nil {
			return "", err
		}
		if revision == 0 {
			if err := s.publishMediaSourceVersionTx(ctx, tx, MediaSourceVersionInput{
				ID: sourceVersionID, SourceID: request.SourceID,
				ContentVersionID: request.ContentVersion.ID, SourceSHA256: request.ContentVersion.BlobHash,
				CaptureJSON: request.CaptureJSON, ClaimSHA256: request.ClaimSHA256,
				Revision: 1, ExpectedHeadRevision: 0, SourceBytes: request.ContentVersion.Size,
				BindOccurrenceIDs: []string{occurrenceID},
			}); err != nil {
				return "", err
			}
		}
		operationState, coverageState := mediaOperationSucceeded, "unprocessed"
		if request.ProcessingProfile != "" {
			operationState, coverageState = mediaOperationQueued, "pending"
		}
		receipt := MediaPublicationReceipt{
			VaultUID: s.vaultID, SourceID: request.SourceID, SourceVersionID: sourceVersionID,
			ContentVersionID: request.ContentVersion.ID, OccurrenceID: occurrenceID,
			OperationID: request.Operation.ID, CoverageState: coverageState, OperationState: operationState,
			ProcessingNodeID: request.ContentVersion.NodeID, ProcessingProfile: request.ProcessingProfile,
			SuppliedInputID: request.SuppliedInputID, ProcessingPrincipal: request.ProcessingPrincipal,
			ProcessingScope:              request.ProcessingScope,
			ProcessingProfileFingerprint: request.ProcessingProfileFingerprint,
			ProcessingAuthorization:      request.ProcessingAuthorization,
		}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(receiptJSON))
	if err != nil {
		return MediaPublicationReceipt{}, fmt.Errorf("decoding retained media receipt: %w", err)
	}
	return receipt, nil
}

// SetMediaProcessingJob binds the durable rendition waiter to the already
// recorded media operation. Repeating the same update is idempotent.
func (s *Store) SetMediaProcessingJob(
	ctx context.Context, operationID, principal, jobID string,
) (MediaPublicationReceipt, error) {
	if !canonical.IsSHA256Hex(jobID) {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	return s.updateMediaProcessingReceipt(ctx, operationID, principal, func(receipt *MediaPublicationReceipt) error {
		if receipt.JobID != "" && receipt.JobID != jobID {
			return ErrMediaOperationConflict
		}
		receipt.JobID = jobID
		receipt.OperationState = mediaOperationQueued
		receipt.CoverageState = "pending"
		return nil
	})
}

// FailMediaProcessing marks only the requested provider action unavailable;
// retained source and occurrence authority remain successful and readable.
func (s *Store) FailMediaProcessing(
	ctx context.Context, operationID, principal string,
) (MediaPublicationReceipt, error) {
	return s.updateMediaProcessingReceipt(ctx, operationID, principal, func(receipt *MediaPublicationReceipt) error {
		receipt.OperationState = mediaOperationFailed
		receipt.CoverageState = mediaCoverageUnavailable
		return nil
	})
}

// FinishMediaProcessing records the terminal rendition/embedding projection
// while keeping the immutable retention receipt fields unchanged.
func (s *Store) FinishMediaProcessing(
	ctx context.Context, operationID, principal string, succeeded bool,
) (MediaPublicationReceipt, error) {
	return s.updateMediaProcessingReceipt(ctx, operationID, principal, func(receipt *MediaPublicationReceipt) error {
		if succeeded {
			receipt.OperationState = mediaOperationSucceeded
			receipt.CoverageState = "transcribed"
		} else {
			receipt.OperationState = mediaOperationFailed
			receipt.CoverageState = mediaCoverageUnavailable
		}
		return nil
	})
}

// MediaProcessingContinuations returns a bounded deterministic page of
// durable supplied-media processing intents that still need supervision.
func (s *Store) MediaProcessingContinuations(
	ctx context.Context, limit int, principals ...string,
) ([]MediaPublicationReceipt, error) {
	if limit < 1 || limit > 250 {
		return nil, errors.New("media continuation limit must be between 1 and 250")
	}
	query := `SELECT receipt_json FROM media_operations
		WHERE verb IN ('submit_supplied_media','retry_media') AND state IN ('queued','running')`
	args := []any{}
	if len(principals) > 0 {
		query += ` AND principal=?`
		args = append(args, principals[0])
	}
	query += ` ORDER BY CASE WHEN receipt_json LIKE '%"job_id"%' THEN 1 ELSE 0 END,
		updated_at,operation_id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]MediaPublicationReceipt, 0, limit)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding media continuation: %w", err)
		}
		if receipt.ProcessingProfile == "" {
			return nil, errors.New("queued media operation lacks processing selection")
		}
		result = append(result, receipt)
	}
	return result, rows.Err()
}

func (s *Store) updateMediaProcessingReceipt(
	ctx context.Context,
	operationID, principal string,
	mutate func(*MediaPublicationReceipt) error,
) (MediaPublicationReceipt, error) {
	var result MediaPublicationReceipt
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var storedPrincipal, receiptJSON string
		if err := tx.QueryRowContext(ctx, `SELECT principal,receipt_json FROM media_operations
			WHERE operation_id=? AND verb IN ('submit_supplied_media','retry_media')`, operationID).Scan(
			&storedPrincipal, &receiptJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if storedPrincipal != principal {
			return ErrNotFound
		}
		var err error
		result, err = canonical.Decode[MediaPublicationReceipt]([]byte(receiptJSON))
		if err != nil {
			return fmt.Errorf("decoding media processing receipt: %w", err)
		}
		if result.ProcessingProfile == "" {
			return ErrMediaOperationConflict
		}
		if err := mutate(&result); err != nil {
			return err
		}
		encoded, err := canonical.Marshal(result)
		if err != nil {
			return err
		}
		if err := validateMediaReceipt(string(encoded)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE media_operations SET state=?,receipt_json=?,updated_at=?
			WHERE operation_id=? AND principal=?`, result.OperationState, string(encoded), nowRFC3339(),
			operationID, principal)
		return err
	})
	return result, err
}

func ensureSuppliedMediaSourceTx(ctx context.Context, tx *sql.Tx, sourceID string) error {
	var kind, provider, originScope, identity string
	err := tx.QueryRowContext(ctx, `SELECT kind,provider,origin_scope,identity_sha256
		FROM media_sources WHERE source_id=?`, sourceID).Scan(&kind, &provider, &originScope, &identity)
	if err == nil {
		if kind != "supplied_media" || provider != "" || originScope != "" || identity != sourceID {
			return ErrMediaSourceConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_sources(
		source_id,kind,provider,origin_scope,identity_sha256,created_at
	) VALUES(?, 'supplied_media', '', '', ?, ?)`, sourceID, sourceID, nowRFC3339()); err != nil {
		return fmt.Errorf("creating supplied media source: %w", err)
	}
	return nil
}
