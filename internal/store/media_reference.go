package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

type MediaReferencePublicationRequest struct {
	Operation                             MediaOperation
	Provider, OriginScope, IdentitySHA256 string
	Outcome                               string
	Occurrence                            MediaOccurrenceInput
}

func (s *Store) RetainMediaReference(
	ctx context.Context, request MediaReferencePublicationRequest,
) (MediaPublicationReceipt, error) {
	outcome := request.Outcome
	if outcome == "" {
		outcome = "access_required"
	}
	if outcome != "access_required" && outcome != "unsupported" {
		return MediaPublicationReceipt{}, ErrMediaOperationConflict
	}
	receiptRaw, err := s.withMediaOperation(ctx, request.Operation, func(tx *sql.Tx) (string, error) {
		var existingKind, existingProvider, existingScope, existingIdentity string
		err := tx.QueryRowContext(ctx, `SELECT kind,provider,origin_scope,identity_sha256
			FROM media_sources WHERE source_id=?`, request.Operation.SourceID).Scan(
			&existingKind, &existingProvider, &existingScope, &existingIdentity)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO media_sources(
				source_id,kind,provider,origin_scope,identity_sha256,created_at
			) VALUES(?,'remote_recording',?,?,?,?)`, request.Operation.SourceID,
				request.Provider, request.OriginScope, request.IdentitySHA256, nowRFC3339())
		} else if err == nil && (existingKind != "remote_recording" || existingProvider != request.Provider ||
			existingScope != request.OriginScope || existingIdentity != request.IdentitySHA256) {
			return "", ErrMediaSourceConflict
		}
		if err != nil {
			return "", err
		}
		existing, found, err := mediaOccurrenceByRevisionTx(ctx, tx,
			request.Occurrence.Principal, request.Occurrence.Ref, request.Occurrence.Revision)
		if err != nil {
			return "", err
		}
		if found {
			request.Occurrence.SourceVersionID = existing.SourceVersionID
		}
		if err := s.declareMediaOccurrenceTx(ctx, tx, request.Occurrence); err != nil {
			return "", err
		}
		receipt := MediaPublicationReceipt{VaultUID: s.vaultID, SourceID: request.Operation.SourceID,
			OccurrenceID: request.Occurrence.ID, OperationID: request.Operation.ID,
			Outcome: outcome, CoverageState: "unprocessed", OperationState: mediaOperationSucceeded}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}
