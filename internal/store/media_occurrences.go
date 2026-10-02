package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

// ErrMediaOccurrenceConflict reports reuse of a caller revision with changed
// immutable claims.
var ErrMediaOccurrenceConflict = errors.New("media occurrence revision conflict")

// MediaOccurrenceInput is one caller's immutable claim about a source. An
// empty SourceVersionID records an occurrence awaiting its first exact bytes.
type MediaOccurrenceInput struct {
	ID, SourceID, SourceVersionID, Principal, Ref, Revision string
	Filename, PersonRef, SpeakerLabel, MessageJSON          string
}

func (s *Store) RecordMediaOccurrence(
	ctx context.Context, op MediaOperation, in MediaOccurrenceInput,
) (MediaPublicationReceipt, error) {
	receiptRaw, err := s.withMediaOperation(ctx, op, func(tx *sql.Tx) (string, error) {
		if err := s.declareMediaOccurrenceTx(ctx, tx, in); err != nil {
			return "", err
		}
		receipt := MediaPublicationReceipt{VaultUID: s.vaultID, SourceID: in.SourceID,
			SourceVersionID: in.SourceVersionID, OccurrenceID: in.ID, OperationID: op.ID,
			OperationState: mediaOperationSucceeded, CoverageState: "unprocessed"}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}

func (s *Store) RecordMediaOccurrenceRevocation(
	ctx context.Context, op MediaOperation, occurrenceID, expectedRevision string,
) (MediaPublicationReceipt, error) {
	// The source binding is immutable. Resolve it for the operation receipt,
	// then check the caller's revision inside the revocation transaction.
	err := s.db.QueryRowContext(ctx, `SELECT source_id FROM media_occurrences
		WHERE occurrence_id=? AND caller_principal=?`, occurrenceID, op.Principal).Scan(&op.SourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaPublicationReceipt{}, ErrNotFound
	}
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	receiptRaw, err := s.withMediaOperation(ctx, op, func(tx *sql.Tx) (string, error) {
		var revision string
		err := tx.QueryRowContext(ctx, `SELECT caller_revision FROM media_occurrences
			WHERE occurrence_id=? AND caller_principal=?`, occurrenceID, op.Principal).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		if revision != expectedRevision {
			return "", ErrMediaOccurrenceConflict
		}
		if err := revokeMediaOccurrenceTx(ctx, tx, op.Principal, occurrenceID); err != nil {
			return "", err
		}
		receipt := MediaPublicationReceipt{VaultUID: s.vaultID, SourceID: op.SourceID,
			OccurrenceID: occurrenceID, OperationID: op.ID, OperationState: mediaOperationSucceeded,
			CoverageState: "unprocessed"}
		encoded, err := canonical.Marshal(receipt)
		return string(encoded), err
	})
	if err != nil {
		return MediaPublicationReceipt{}, err
	}
	return canonical.Decode[MediaPublicationReceipt]([]byte(receiptRaw))
}

func (s *Store) declareMediaOccurrenceTx(ctx context.Context, tx *sql.Tx, in MediaOccurrenceInput) error {
	if err := validateMediaOccurrenceInput(in); err != nil {
		return err
	}
	if err := validateMediaOccurrenceSourceTx(ctx, tx, in.SourceID, in.SourceVersionID); err != nil {
		return err
	}
	stored, found, err := mediaOccurrenceByRevisionTx(ctx, tx, in.Principal, in.Ref, in.Revision)
	if err != nil {
		return err
	}
	if found {
		if stored != in {
			return ErrMediaOccurrenceConflict
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_occurrences(
			occurrence_id,source_id,source_version_id,caller_principal,caller_occurrence_ref,
			caller_revision,caller_filename,caller_person_ref,speaker_label,message_json,
			visible,first_seen_at,revoked_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,1,?,NULL)`, in.ID, in.SourceID, nullableMediaID(in.SourceVersionID),
		in.Principal, in.Ref, in.Revision, in.Filename, in.PersonRef, in.SpeakerLabel,
		in.MessageJSON, nowRFC3339()); err != nil {
		if s.driver.IsUniqueViolation(err) {
			return ErrMediaOccurrenceConflict
		}
		return fmt.Errorf("declaring media occurrence: %w", err)
	}
	return nil
}

func revokeMediaOccurrenceTx(ctx context.Context, tx *sql.Tx, principal, id string) error {
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT visible FROM media_occurrences
			WHERE occurrence_id=? AND caller_principal=?`, id, principal).Scan(&visible); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if visible != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE media_occurrences SET visible=0,revoked_at=?
				WHERE occurrence_id=? AND caller_principal=?`, nowRFC3339(), id, principal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_heads WHERE attachment_id IN (
			SELECT a.attachment_id FROM rendition_attachments a
			JOIN rendition_jobs j ON j.job_id=a.build_id
			JOIN media_input_artifacts i ON i.occurrence_id=?
			WHERE j.execution_identity_json LIKE '%"input_binding":"' || i.input_id || '"%'
		)`, id); err != nil {
			return fmt.Errorf("revoking input-bound rendition heads: %w", err)
		}
	}
	return nil
}

func validateMediaOccurrenceInput(in MediaOccurrenceInput) error {
	for _, field := range []struct {
		name       string
		value      string
		max        int
		allowEmpty bool
	}{
		{"media occurrence ID", in.ID, 256, false},
		{"media source ID", in.SourceID, 256, false},
		{"media source version ID", in.SourceVersionID, 256, true},
		{"media principal", in.Principal, 256, false},
		{"media occurrence reference", in.Ref, 8192, false},
		{"media caller revision", in.Revision, 256, false},
		{"media filename", in.Filename, 1024, true},
		{"media person reference", in.PersonRef, 1024, true},
		{"media speaker label", in.SpeakerLabel, 128, true},
	} {
		if err := validateBoundedMediaText(field.name, field.value, field.max, field.allowEmpty); err != nil {
			return err
		}
	}
	if len(in.MessageJSON) == 0 || len(in.MessageJSON) > 64<<10 || !utf8.ValidString(in.MessageJSON) {
		return errors.New("media message claim must be bounded canonical JSON")
	}
	if err := canonicalJSONText(in.MessageJSON, "media message claim"); err != nil {
		return errors.New("media message claim must be bounded canonical JSON")
	}
	return nil
}

func validateBoundedMediaText(name, value string, maxBytes int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || !utf8.ValidString(value) || len(value) > maxBytes {
		return fmt.Errorf("%s must be bounded UTF-8 of at most %d bytes", name, maxBytes)
	}
	return nil
}

func validateMediaOccurrenceSourceTx(ctx context.Context, tx *sql.Tx, sourceID, sourceVersionID string) error {
	var found string
	if sourceVersionID == "" {
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM media_sources WHERE source_id=?`, sourceID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT source_id FROM media_source_versions
		WHERE source_version_id=?`, sourceVersionID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if found != sourceID {
		return ErrMediaOccurrenceConflict
	}
	return nil
}

func mediaOccurrenceByRevisionTx(
	ctx context.Context, tx *sql.Tx, principal, reference, revision string,
) (MediaOccurrenceInput, bool, error) {
	var found MediaOccurrenceInput
	var sourceVersion sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT occurrence_id,source_id,source_version_id,
		caller_principal,caller_occurrence_ref,caller_revision,caller_filename,
		caller_person_ref,speaker_label,message_json FROM media_occurrences
		WHERE caller_principal=? AND caller_occurrence_ref=? AND caller_revision=?`,
		principal, reference, revision).Scan(&found.ID, &found.SourceID, &sourceVersion,
		&found.Principal, &found.Ref, &found.Revision, &found.Filename, &found.PersonRef,
		&found.SpeakerLabel, &found.MessageJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaOccurrenceInput{}, false, nil
	}
	if err != nil {
		return MediaOccurrenceInput{}, false, err
	}
	found.SourceVersionID = sourceVersion.String
	return found, true, nil
}

func nullableMediaID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
