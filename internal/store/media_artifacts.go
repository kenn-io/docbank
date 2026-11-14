package store

import (
	"context"
	"database/sql"
	"errors"
	"path"

	"go.kenn.io/docbank/internal/canonical"
)

type MediaInputArtifactRequest struct {
	Operation                                  MediaOperation
	InputID, OccurrenceID, SourceVersionID     string
	VirtualPath, MediaType                     string
	ByteLength                                 int64
	Physical                                   BlobPhysical
	Kind, Origin, Provider, Language, InputSHA string
}

func (s *Store) QueueMediaRetry(
	ctx context.Context, op MediaOperation, receipt MediaPublicationReceipt,
) (MediaPublicationReceipt, error) {
	receiptRaw, err := s.withMediaOperation(ctx, op, func(*sql.Tx) (string, error) {
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}

func (s *Store) ImportMediaInputArtifact(
	ctx context.Context, request MediaInputArtifactRequest,
) (MediaPublicationReceipt, error) {
	if err := ValidateMediaInputAuthority(request.Kind, request.Origin, request.Provider,
		request.Language, request.InputSHA); err != nil {
		return MediaPublicationReceipt{}, err
	}
	if request.ByteLength < 1 || !path.IsAbs(request.VirtualPath) ||
		path.Clean(request.VirtualPath) != request.VirtualPath {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	receiptRaw, err := s.withMediaOperation(ctx, request.Operation, func(tx *sql.Tx) (string, error) {
		var sourceID, sourceVersionID string
		var visible int
		if err := tx.QueryRowContext(ctx, `SELECT source_id,COALESCE(source_version_id,''),visible
			FROM media_occurrences WHERE occurrence_id=$1 AND caller_principal=$2`, storedString(s.postgresSchema != "", request.OccurrenceID), storedString(s.postgresSchema != "", request.Operation.Principal)).Scan(&sourceID, &sourceVersionID, &visible); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNotFound
			}
			return "", err
		}
		if visible == 0 || sourceID != request.Operation.SourceID || sourceVersionID == "" ||
			sourceVersionID != request.SourceVersionID {
			return "", ErrNotFound
		}
		versionID, err := s.retainMediaInputArtifactTx(ctx, tx, request, "")
		if err != nil {
			return "", err
		}
		receipt := MediaPublicationReceipt{VaultUID: s.vaultID, SourceID: sourceID,
			SourceVersionID: sourceVersionID, ContentVersionID: versionID,
			OccurrenceID: request.OccurrenceID, OperationID: request.Operation.ID,
			OperationState: mediaOperationSucceeded, CoverageState: "unprocessed", SuppliedInputID: request.InputID}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}

// RetainRemoteRecordingMedia binds one inspected original to a caller-visible
// remote occurrence and records the original as its first media input.
func (s *Store) RetainRemoteRecordingMedia(
	ctx context.Context, request MediaInputArtifactRequest,
) (MediaPublicationReceipt, error) {
	if request.Operation.Verb != "import_recording_artifact" || request.Operation.SourceID == "" ||
		request.Kind != "media" || request.ByteLength < 1 ||
		!canonical.IsSHA256Hex(request.InputID) || !canonical.IsSHA256Hex(request.InputSHA) {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	if err := ValidateMediaInputAuthority(request.Kind, request.Origin, request.Provider,
		request.Language, request.InputSHA); err != nil {
		return MediaPublicationReceipt{}, err
	}
	if err := validateBoundedMediaText("media input media type", request.MediaType, 128, false); err != nil ||
		!path.IsAbs(request.VirtualPath) || path.Clean(request.VirtualPath) != request.VirtualPath {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	receiptRaw, err := s.withMediaOperation(ctx, request.Operation, func(tx *sql.Tx) (string, error) {
		var sourceID, sourceVersionID, sourceKind, captureJSON string
		var visible int
		err := tx.QueryRowContext(ctx, `SELECT o.source_id,COALESCE(o.source_version_id,''),o.visible,
			s.kind,o.message_json FROM media_occurrences o
			JOIN media_sources s ON s.source_id=o.source_id
			WHERE o.occurrence_id=$1 AND o.caller_principal=$2`, storedString(s.postgresSchema != "", request.OccurrenceID), storedString(s.postgresSchema != "", request.Operation.Principal)).Scan(
			&sourceID, &sourceVersionID, &visible, &sourceKind, &captureJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		if visible == 0 || sourceID != request.Operation.SourceID || sourceKind != "remote_recording" {
			return "", ErrNotFound
		}
		if request.SourceVersionID != "" && request.SourceVersionID != sourceVersionID {
			return "", ErrNotFound
		}

		var contentVersionID string
		if sourceVersionID != "" {
			var blobHash, mediaType string
			var sourceBytes int64
			err := tx.QueryRowContext(ctx, `SELECT v.content_version_id,c.blob_hash,c.size,
				COALESCE(c.mime_type,'') FROM media_source_versions v
				JOIN content_versions c ON c.version_id=v.content_version_id
				WHERE v.source_version_id=$1 AND v.source_id=$2`, storedString(s.postgresSchema != "", sourceVersionID), storedString(s.postgresSchema != "", sourceID)).Scan(
				&contentVersionID, &blobHash, &sourceBytes, &mediaType)
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNotFound
			}
			if err != nil {
				return "", err
			}
			if blobHash != request.InputSHA || sourceBytes != request.ByteLength || mediaType != request.MediaType {
				return "", ErrMediaSourceConflict
			}
		} else {
			err := tx.QueryRowContext(ctx, `SELECT v.source_version_id,v.content_version_id
				FROM media_source_versions v JOIN content_versions c
				ON c.version_id=v.content_version_id
				WHERE v.source_id=$1 AND c.blob_hash=$2 AND c.size=$3 AND COALESCE(c.mime_type,'')=$4
				ORDER BY v.revision DESC LIMIT 1`, storedString(s.postgresSchema != "", sourceID), request.InputSHA, request.ByteLength, storedString(s.postgresSchema != "", request.MediaType)).Scan(&sourceVersionID, &contentVersionID)
			if errors.Is(err, sql.ErrNoRows) {
				content, sealErr := s.sealMediaContentTx(ctx, tx, request.VirtualPath, ContentVersion{
					BlobHash: request.InputSHA, Size: request.ByteLength, MimeType: request.MediaType}, request.Physical)
				if sealErr != nil {
					return "", sealErr
				}
				var currentRevision int64
				if headErr := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM media_source_versions WHERE source_id=$1`, storedString(s.postgresSchema != "", sourceID)).Scan(&currentRevision); headErr != nil {
					return "", headErr
				}
				sourceVersionID, sealErr = newUUIDv4()
				if sealErr != nil {
					return "", sealErr
				}
				if err := s.publishMediaSourceVersionTx(ctx, tx, MediaSourceVersionInput{
					ID: sourceVersionID, SourceID: sourceID, ContentVersionID: content.ID,
					CaptureJSON: captureJSON,
					Revision:    currentRevision + 1, ExpectedHeadRevision: currentRevision,
					BindOccurrenceIDs: []string{request.OccurrenceID},
				}); err != nil {
					return "", err
				}
				contentVersionID = content.ID
			} else if err != nil {
				return "", err
			} else {
				result, updateErr := tx.ExecContext(ctx, `UPDATE media_occurrences SET source_version_id=$1
					WHERE occurrence_id=$2 AND source_id=$3 AND caller_principal=$4 AND visible=1
						AND source_version_id IS NULL`, storedString(s.postgresSchema != "", sourceVersionID), storedString(s.postgresSchema != "", request.OccurrenceID), storedString(s.postgresSchema != "", sourceID), storedString(s.postgresSchema != "", request.Operation.Principal))
				if updateErr != nil {
					return "", updateErr
				}
				changed, updateErr := result.RowsAffected()
				if updateErr != nil {
					return "", updateErr
				}
				if changed != 1 {
					return "", ErrNotFound
				}
			}
		}

		if err := s.EnsureBlobTx(ctx, tx, request.InputSHA, request.ByteLength, request.Physical); err != nil {
			return "", err
		}

		request.SourceVersionID = sourceVersionID
		if _, err := s.retainMediaInputArtifactTx(ctx, tx, request, contentVersionID); err != nil {
			if s.isUniqueViolation(err) {
				return "", ErrMediaOperationConflict
			}
			return "", err
		}
		receipt := MediaPublicationReceipt{VaultUID: s.vaultID, SourceID: sourceID,
			SourceVersionID: sourceVersionID, ContentVersionID: contentVersionID,
			OccurrenceID: request.OccurrenceID, OperationID: request.Operation.ID,
			Outcome: "content_available", CoverageState: "unprocessed",
			OperationState: mediaOperationSucceeded, SuppliedInputID: request.InputID}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}

// retainMediaInputArtifactTx reuses exact input authority or seals a new input.
// A supplied content version keeps a remote original on its source version.
func (s *Store) retainMediaInputArtifactTx(
	ctx context.Context, tx *sql.Tx, request MediaInputArtifactRequest, contentVersionID string,
) (string, error) {
	var versionID string
	var matches bool
	err := tx.QueryRowContext(ctx, `SELECT i.content_version_id,
		i.occurrence_id=$1 AND i.source_id=$2 AND i.source_version_id=$3
		AND i.kind=$4 AND i.origin=$5 AND i.provider=$6 AND i.language=$7 AND i.input_sha256=$8
		AND c.blob_hash=$9 AND c.size=$10 AND COALESCE(c.mime_type,'')=$11
		FROM media_input_artifacts i JOIN content_versions c ON c.version_id=i.content_version_id
		WHERE i.input_id=$12`, storedString(s.postgresSchema != "", request.OccurrenceID), storedString(s.postgresSchema != "", request.Operation.SourceID), storedString(s.postgresSchema != "", request.SourceVersionID), request.Kind, storedString(s.postgresSchema != "", request.Origin), storedString(s.postgresSchema != "", request.Provider), storedString(s.postgresSchema != "", request.Language), request.InputSHA, request.InputSHA,
		request.ByteLength, storedString(s.postgresSchema != "", request.MediaType), storedString(s.postgresSchema != "", request.InputID)).Scan(&versionID, &matches)
	if err == nil {
		if !matches || (contentVersionID != "" && contentVersionID != versionID) {
			return "", ErrMediaOperationConflict
		}
		return versionID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if contentVersionID == "" {
		version, err := s.sealMediaContentTx(ctx, tx, request.VirtualPath, ContentVersion{
			BlobHash: request.InputSHA, Size: request.ByteLength, MimeType: request.MediaType}, request.Physical)
		if err != nil {
			return "", err
		}
		contentVersionID = version.ID
	}
	err = insertMediaInputArtifactTx(ctx, tx, metadataMediaInputArtifact{InputID: request.InputID, OccurrenceID: request.OccurrenceID, SourceID: request.Operation.SourceID, SourceVersionID: &request.SourceVersionID, ContentVersionID: contentVersionID, Kind: request.Kind, Origin: request.Origin, Provider: request.Provider, Language: request.Language, InputSHA256: request.InputSHA, CreatedAt: nowRFC3339()}, s.postgresSchema != "")

	return contentVersionID, err
}

// sealMediaContentTx commits core content and its source fact only with
// media authority and the operation receipt. Conflicts roll back directory creation too.
func (s *Store) sealMediaContentTx(
	ctx context.Context, tx *sql.Tx, virtualPath string, content ContentVersion, physical BlobPhysical,
) (ContentVersion, error) {
	if content.Size < 1 || !path.IsAbs(virtualPath) || path.Clean(virtualPath) != virtualPath {
		return ContentVersion{}, ErrMediaOperationConflict
	}
	const description = "Retained supplied media"
	reference := "sha256:" + content.BlobHash
	existing, err := s.nodeByPath(ctx, tx, virtualPath)
	if err == nil {
		if existing.IsDir() || existing.BlobHash != content.BlobHash ||
			existing.Size != content.Size || existing.MimeType != content.MimeType {
			return ContentVersion{}, ErrMediaOperationConflict
		}
		matched, err := activeProvenanceMatchesTx(ctx, tx, existing.ID,
			callerSuppliedSourceKindPrefix+"media", description, reference, "")
		if err != nil {
			return ContentVersion{}, err
		}
		if !matched {
			return ContentVersion{}, ErrMediaOperationConflict
		}
		receipt, err := s.confirmContentWithReceiptTx(ctx, tx, existing, existing.Revision,
			content.BlobHash, content.Size, content.MimeType, physical)
		return receipt.Version, err
	}
	if !errors.Is(err, ErrNotFound) {
		return ContentVersion{}, err
	}
	run, err := s.BeginCallerSuppliedIngest(ctx, "media", description)
	if err != nil {
		return ContentVersion{}, err
	}
	segments, err := normalizeIngestDirectorySegments(virtualPath, splitPath(path.Dir(virtualPath)))
	if err != nil {
		return ContentVersion{}, err
	}
	plan := IngestDirectoryPlan{anchorID: s.rootID, segments: segments}
	receipt, _, _, err := s.ingestFileTx(ctx, tx, run, 0, path.Base(virtualPath),
		content.BlobHash, content.Size, content.MimeType, reference, "",
		ingestFileOptions{exact: true, completeReceipt: true, directoryPlan: &plan}, physical)
	return receipt.Version, err
}
