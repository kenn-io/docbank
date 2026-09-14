package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/audit"
)

const provenanceVersionBindingBasis = "ingest:exact-version"

// ProvenanceVersionBinding pins an observed provenance fact to the exact
// immutable content version that was current when the observation occurred.
type ProvenanceVersionBinding struct {
	ProvenanceIdentity string
	ContentVersionID   string
	ObservedAt         string
	BasisRef           string
}

func provenanceVersionBindingAuditRecord(binding ProvenanceVersionBinding) (audit.Record, error) {
	provenanceIdentity, err := audit.DigestHex(binding.ProvenanceIdentity)
	if err != nil {
		return audit.Record{}, err
	}
	contentVersionID, err := audit.UUID(binding.ContentVersionID)
	if err != nil {
		return audit.Record{}, err
	}
	observedAt, err := audit.Timestamp(binding.ObservedAt)
	if err != nil {
		return audit.Record{}, err
	}
	basisRef, err := audit.Text(binding.BasisRef)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: metadataProvenanceVersionBindingType, Fields: []audit.Field{
		{Name: "provenance_identity", Value: provenanceIdentity},
		{Name: "content_version_id", Value: contentVersionID},
		{Name: "observed_at", Value: observedAt},
		{Name: "basis_ref", Value: basisRef},
	}}, nil
}

func validateAuditedProvenanceVersionBinding(
	record audit.Record, provenanceIdentity, contentVersionID, observedAt string,
) error {
	if record.Kind != metadataProvenanceVersionBindingType {
		return errors.New("provenance binding attachment has the wrong record kind")
	}
	if err := requireAuditDigest(record, "provenance_identity", provenanceIdentity); err != nil {
		return errors.New("provenance binding does not identify its observed fact")
	}
	if err := requireAuditUUID(record, "content_version_id", contentVersionID); err != nil {
		return errors.New("provenance binding does not identify the exact observed version")
	}
	recordedObservedAt, err := auditTimestampField(record, "observed_at")
	if err != nil || recordedObservedAt != observedAt {
		return errors.New("provenance binding observation time does not match its operation")
	}
	if err := requireAuditText(record, "basis_ref", provenanceVersionBindingBasis); err != nil {
		return errors.New("provenance binding has an invalid basis")
	}
	return nil
}

func bindProvenanceVersionTx(
	ctx context.Context, tx *sql.Tx, binding ProvenanceVersionBinding,
) error {
	if _, err := time.Parse(time.RFC3339Nano, binding.ObservedAt); err != nil {
		return fmt.Errorf("validating provenance binding observation time: %w", err)
	}
	if binding.BasisRef != provenanceVersionBindingBasis {
		return fmt.Errorf("invalid provenance binding basis %q", binding.BasisRef)
	}
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO provenance_version_bindings(
			provenance_identity,content_version_id,observed_at,basis_ref
		)
		SELECT ?,?,?,?
		WHERE EXISTS(
			SELECT 1 FROM provenance p
			JOIN content_versions cv ON cv.node_id=p.node_id
			WHERE p.identity=? AND cv.version_id=?
		)
		ON CONFLICT(provenance_identity,content_version_id) DO NOTHING`,
		binding.ProvenanceIdentity, binding.ContentVersionID,
		binding.ObservedAt, binding.BasisRef,
		binding.ProvenanceIdentity, binding.ContentVersionID,
	)
	if err != nil {
		return err
	}
	var observedAt, basisRef string
	if err := tx.QueryRowContext(ctx, `
		SELECT observed_at,basis_ref FROM provenance_version_bindings
		WHERE provenance_identity=? AND content_version_id=?`,
		binding.ProvenanceIdentity, binding.ContentVersionID,
	).Scan(&observedAt, &basisRef); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("provenance binding must identify a version of the same node: %w", ErrProvenanceMismatch)
	} else if err != nil {
		return err
	}
	if observedAt != binding.ObservedAt || basisRef != binding.BasisRef {
		return errors.New("provenance binding conflict")
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	_, err = invalidateDocumentEventsForVersionsTx(ctx, tx, []string{binding.ContentVersionID})
	return err
}

func invalidateSupersededProvenanceEventsTx(ctx context.Context, tx *sql.Tx, identity string) error {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE predecessors(identity,supersedes) AS (
		SELECT identity,supersedes FROM provenance WHERE identity=?
		UNION ALL
		SELECT p.identity,p.supersedes FROM provenance p
		JOIN predecessors prior ON p.identity=prior.supersedes
	)
	SELECT DISTINCT b.content_version_id FROM provenance_version_bindings b
	JOIN predecessors p ON p.identity=b.provenance_identity`, identity)
	if err != nil {
		return fmt.Errorf("reading superseded provenance versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var versionIDs []string
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			return fmt.Errorf("scanning superseded provenance version: %w", err)
		}
		versionIDs = append(versionIDs, versionID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading superseded provenance versions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing superseded provenance versions: %w", err)
	}
	if len(versionIDs) == 0 {
		return nil
	}
	_, err = invalidateDocumentEventsForVersionsTx(ctx, tx, versionIDs)
	return err
}

// ProvenanceVersionBindings returns the provenance facts bound to one exact
// content version in stable identity order.
func (s *Store) ProvenanceVersionBindings(
	ctx context.Context, contentVersionID string,
) ([]ProvenanceVersionBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provenance_identity,content_version_id,observed_at,basis_ref
		FROM provenance_version_bindings
		WHERE content_version_id=?
		ORDER BY provenance_identity`, contentVersionID)
	if err != nil {
		return nil, fmt.Errorf("reading provenance version bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	bindings := make([]ProvenanceVersionBinding, 0)
	for rows.Next() {
		var binding ProvenanceVersionBinding
		if err := rows.Scan(
			&binding.ProvenanceIdentity, &binding.ContentVersionID,
			&binding.ObservedAt, &binding.BasisRef,
		); err != nil {
			return nil, fmt.Errorf("scanning provenance version binding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading provenance version bindings: %w", err)
	}
	return bindings, nil
}
