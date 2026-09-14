package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestFileWithMembership performs one logical operational import. When the
// bytes already identify an existing node, it records that node as a member of
// run without creating a content version or reporting added content.
func (s *Store) IngestFileWithMembership(
	ctx context.Context, run IngestRun, parentID int64,
	name, blobHash string, size int64, mimeType, originalPath, originalMtime string,
	physical ...BlobPhysical,
) (Node, bool, error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return Node{}, false, err
	}
	receipt, added, _, err := s.ingestFile(
		ctx, run, parentID, name, blobHash, size, mimeType, originalPath, originalMtime,
		ingestFileOptions{observeMembership: true}, physical...,
	)
	return receipt.Node, added, err
}

// IngestFileWithMembershipPlanned binds directory resolution and creation to
// the transaction that publishes the labeled filesystem observation.
func (s *Store) IngestFileWithMembershipPlanned(
	ctx context.Context, run IngestRun, plan IngestDirectoryPlan,
	name, blobHash string, size int64, mimeType, originalPath, originalMtime string,
	physical ...BlobPhysical,
) (Node, bool, IngestDirectoryResolution, error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return Node{}, false, IngestDirectoryResolution{}, err
	}
	if run.initialLabel == nil {
		return Node{}, false, IngestDirectoryResolution{},
			errors.New("planned filesystem ingest requires an initial label")
	}
	receipt, added, resolution, err := s.ingestFile(
		ctx, run, 0, name, blobHash, size, mimeType, originalPath, originalMtime,
		ingestFileOptions{observeMembership: true, directoryPlan: &plan}, physical...,
	)
	return receipt.Node, added, resolution, err
}

// IngestFileExactPlanned is the exact-create form of a planned labeled
// filesystem import.
func (s *Store) IngestFileExactPlanned(
	ctx context.Context, run IngestRun, plan IngestDirectoryPlan,
	name, blobHash string, size int64, mimeType, originalPath, originalMtime string,
	physical ...BlobPhysical,
) (Node, IngestDirectoryResolution, error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return Node{}, IngestDirectoryResolution{}, err
	}
	if run.initialLabel == nil {
		return Node{}, IngestDirectoryResolution{},
			errors.New("planned filesystem ingest requires an initial label")
	}
	receipt, _, resolution, err := s.ingestFile(
		ctx, run, 0, name, blobHash, size, mimeType, originalPath, originalMtime,
		ingestFileOptions{exact: true, directoryPlan: &plan}, physical...,
	)
	return receipt.Node, resolution, err
}

func requireOperationalIngestRun(run IngestRun) error {
	// Watch imports require the dedicated APIs that maintain source cursors.
	if run.operationalWatch || sourceKindIsEmbedded(run.record.SourceKind) {
		return fmt.Errorf("operational ingest observation rejects source kind %q", run.record.SourceKind)
	}
	return nil
}

// observeOperationalIngestTx publishes one operational membership fact and
// advances the node observation. The caller admits the run in this transaction.
// Identical facts are idempotent within a run.
func (s *Store) observeOperationalIngestTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, prior Node, fact metadataProvenance, ingestAdded bool,
) (Node, error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return Node{}, err
	}
	if err := validateProvenanceRecord(fact); err != nil {
		return Node{}, fmt.Errorf("validating operational ingest observation: %w", err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM provenance WHERE identity=?)`, fact.Identity,
	).Scan(&exists); err != nil {
		return Node{}, fmt.Errorf("checking operational ingest observation: %w", err)
	}
	if exists {
		return prior, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provenance(
		identity,node_id,ingest_id,original_path,original_mtime,supersedes
	) VALUES(?,?,?,?,?,NULL)`, fact.Identity, fact.NodeID, fact.IngestID,
		fact.OriginalPath, fact.OriginalMTime); err != nil {
		return Node{}, fmt.Errorf("recording operational ingest observation: %w", err)
	}
	operation, err := newContentVersionOperation()
	if err != nil {
		return Node{}, err
	}
	binding := ProvenanceVersionBinding{
		ProvenanceIdentity: fact.Identity,
		ContentVersionID:   prior.CurrentVersionID,
		ObservedAt:         operation.recordedAt,
		BasisRef:           provenanceVersionBindingBasis,
	}
	if err := bindProvenanceVersionTx(ctx, tx, binding); err != nil {
		return Node{}, fmt.Errorf("binding operational ingest observation: %w", err)
	}
	if err := bumpRevisionTx(tx, prior.ID, operation.recordedAt); err != nil {
		return Node{}, err
	}
	resulting, err := nodeByIDTx(tx, prior.ID)
	if err != nil {
		return Node{}, err
	}
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return Node{}, err
	}
	if active {
		authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, prior.ID)
		if err != nil {
			return Node{}, err
		}
		if err := persistAuditedIngestObservation(
			ctx, tx, s.vaultID, operation.operationID, operation.recordedAt,
			nodeSequence, authority, scopes, prior, resulting, run.record, fact, &binding, ingestAdded,
		); err != nil {
			return Node{}, err
		}
	}
	return resulting, nil
}
