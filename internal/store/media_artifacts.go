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
	receiptRaw, err := s.withQueuedMediaOperation(ctx, op, func(*sql.Tx) (string, error) {
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
			FROM media_occurrences WHERE occurrence_id=? AND caller_principal=?`,
			request.OccurrenceID, request.Operation.Principal).Scan(&sourceID, &sourceVersionID, &visible); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNotFound
			}
			return "", err
		}
		if visible == 0 || sourceID != request.Operation.SourceID || sourceVersionID == "" ||
			sourceVersionID != request.SourceVersionID {
			return "", ErrNotFound
		}
		var versionID string
		var matches bool
		err := tx.QueryRowContext(ctx, `SELECT i.content_version_id,
			i.occurrence_id=? AND i.source_id=? AND i.source_version_id=?
			AND i.kind=? AND i.origin=? AND i.provider=? AND i.language=? AND i.input_sha256=?
			AND c.blob_hash=? AND c.size=? AND COALESCE(c.mime_type,'')=?
			FROM media_input_artifacts i JOIN content_versions c ON c.version_id=i.content_version_id
			WHERE i.input_id=?`,
			request.OccurrenceID, sourceID, sourceVersionID, request.Kind, request.Origin,
			request.Provider, request.Language, request.InputSHA, request.InputSHA,
			request.ByteLength, request.MediaType, request.InputID).Scan(&versionID, &matches)
		switch {
		case err == nil && !matches:
			return "", ErrMediaOperationConflict
		case errors.Is(err, sql.ErrNoRows):
			version, err := s.sealMediaContentTx(ctx, tx, request.VirtualPath, ContentVersion{
				BlobHash: request.InputSHA, Size: request.ByteLength, MimeType: request.MediaType}, request.Physical)
			if err != nil {
				return "", err
			}
			versionID = version.ID
			if _, err := tx.ExecContext(ctx, `INSERT INTO media_input_artifacts(
				input_id,occurrence_id,source_id,source_version_id,content_version_id,kind,
				origin,provider,language,input_sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				request.InputID, request.OccurrenceID, sourceID, sourceVersionID,
				versionID, request.Kind, request.Origin, request.Provider,
				request.Language, request.InputSHA, nowRFC3339()); err != nil {
				return "", err
			}
		case err != nil:
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
	existing, err := nodeByPath(ctx, tx, s.rootID, virtualPath)
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
		receipt, err := s.confirmContentWithReceiptTx(tx, existing, existing.Revision,
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
