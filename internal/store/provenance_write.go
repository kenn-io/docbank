package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProvenanceAppendInput describes one post-ingest origin assertion. A positive
// IfRevision is required; UnconditionalRev is reserved for embedded callers
// that have already serialized the operation through their own lifecycle.
type ProvenanceAppendInput struct {
	NodeID            int64
	IfRevision        int64
	SourceKind        string
	SourceDescription string
	OriginalPath      string
	OriginalMTime     *string
	Supersedes        *string
}

// ProvenanceAppendResult is the authority committed by AppendNodeProvenance.
type ProvenanceAppendResult struct {
	Node Node
	Path string
	Fact ProvenanceFact
}

// AppendNodeProvenance appends one immutable origin assertion and advances
// the target node revision in the same metadata transaction.
func (s *Store) AppendNodeProvenance(
	ctx context.Context, input ProvenanceAppendInput,
) (ProvenanceAppendResult, error) {
	if input.NodeID < 1 {
		return ProvenanceAppendResult{}, fmt.Errorf("provenance node ID must be positive: %w", ErrNotFound)
	}
	if input.IfRevision < 1 && input.IfRevision != UnconditionalRev {
		return ProvenanceAppendResult{}, errors.New("provenance revision must be positive")
	}
	mtime := ""
	if input.OriginalMTime != nil {
		if *input.OriginalMTime == "" {
			return ProvenanceAppendResult{}, fmt.Errorf("%w: timestamp must not be empty", ErrInvalidProvenanceTime)
		}
		mtime = *input.OriginalMTime
	}
	if err := validateProvenanceSourceFields(
		input.SourceKind, input.SourceDescription, input.OriginalPath, mtime,
	); err != nil {
		return ProvenanceAppendResult{}, err
	}
	if input.Supersedes != nil && *input.Supersedes == "" {
		return ProvenanceAppendResult{}, fmt.Errorf("superseded provenance identity is empty: %w", ErrProvenanceMismatch)
	}
	run, err := s.beginIngest(ctx, embeddedSourceKindPrefix+input.SourceKind, input.SourceDescription, false)
	if err != nil {
		return ProvenanceAppendResult{}, err
	}
	var result ProvenanceAppendResult
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		prior, err := nodeByIDTx(tx, input.NodeID)
		if err != nil {
			return err
		}
		if prior.IsDir() {
			return fmt.Errorf("node %d: %w", input.NodeID, ErrNotFile)
		}
		if input.IfRevision != UnconditionalRev && prior.Revision != input.IfRevision {
			return fmt.Errorf("node %d at revision %d, expected %d: %w",
				input.NodeID, prior.Revision, input.IfRevision, ErrStaleRevision)
		}
		if input.Supersedes != nil {
			if err := validateProvenancePredecessorTx(ctx, tx, input.NodeID, *input.Supersedes); err != nil {
				return err
			}
		}
		activeAudit, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := ensureIngestRunTx(ctx, tx, run); err != nil {
			return err
		}
		provenance := metadataProvenance{
			Type: metadataProvenanceType, NodeID: input.NodeID, IngestID: run.record.ID,
			OriginalPath: input.OriginalPath, OriginalMTime: input.OriginalMTime,
			Supersedes: input.Supersedes,
		}
		provenance.Identity, err = provenanceIdentity(provenance)
		if err != nil {
			return fmt.Errorf("identifying appended provenance: %w", err)
		}
		if err := validateProvenanceRecord(provenance); err != nil {
			return fmt.Errorf("validating appended provenance: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provenance(
			identity,node_id,ingest_id,original_path,original_mtime,supersedes)
			VALUES(?,?,?,?,?,?)`, provenance.Identity, provenance.NodeID, provenance.IngestID,
			provenance.OriginalPath, nullString(provenance.OriginalMTime), nullString(provenance.Supersedes)); err != nil {
			return fmt.Errorf("recording appended provenance: %w", err)
		}
		if err := bumpRevisionTx(tx, input.NodeID, run.record.StartedAt); err != nil {
			return err
		}
		result.Node, err = nodeByIDTx(tx, input.NodeID)
		if err != nil {
			return err
		}
		if result.Node.TrashedAt == nil {
			result.Path, err = pathOf(ctx, tx, input.NodeID)
			if err != nil {
				return err
			}
		}
		result.Fact = ProvenanceFact{
			Identity: provenance.Identity, NodeID: provenance.NodeID, IngestID: run.record.ID,
			IngestStartedAt: run.record.StartedAt, SourceKind: input.SourceKind,
			SourceDescription: input.SourceDescription, OriginalPath: input.OriginalPath,
			OriginalMTime: input.OriginalMTime, Supersedes: input.Supersedes, Active: true,
		}
		if !activeAudit {
			return nil
		}
		authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, input.NodeID)
		if err != nil {
			return err
		}
		return persistAuditedProvenanceAppend(ctx, tx, s.vaultID, run.record.ID,
			run.record.StartedAt, nodeSequence, authority, scopes, prior, result.Node, run.record, provenance)
	})
	if err != nil {
		return ProvenanceAppendResult{}, err
	}
	return result, nil
}

func validateProvenancePredecessorTx(
	ctx context.Context, tx *sql.Tx, nodeID int64, identity string,
) error {
	var predecessorNode int64
	if err := tx.QueryRowContext(ctx,
		`SELECT node_id FROM provenance WHERE identity=?`, identity,
	).Scan(&predecessorNode); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("provenance predecessor %s was not found: %w", identity, ErrProvenanceMismatch)
	} else if err != nil {
		return fmt.Errorf("reading provenance predecessor %s: %w", identity, err)
	}
	if predecessorNode != nodeID {
		return fmt.Errorf("provenance predecessor %s belongs to node %d, not %d: %w",
			identity, predecessorNode, nodeID, ErrProvenanceMismatch)
	}
	var successor int
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM provenance WHERE supersedes=?)`, identity,
	).Scan(&successor); err != nil {
		return fmt.Errorf("checking provenance predecessor %s: %w", identity, err)
	}
	if successor != 0 {
		return fmt.Errorf("provenance predecessor %s is already superseded: %w", identity, ErrProvenanceMismatch)
	}
	return nil
}
