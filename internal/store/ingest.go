package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// IngestRun identifies one logical import and carries the immutable metadata
// that is published with its first imported file. Holding a run grants no
// metadata authority by itself.
type IngestRun struct {
	record metadataIngest
}

// ID returns the stable ingest identity.
func (r IngestRun) ID() string { return r.record.ID }

// BeginIngest prepares an authority-free ingest run. Its metadata is inserted
// atomically with the first file that actually imports, so audited vaults never
// contain a run whose provenance was committed in a separate transaction.
func (s *Store) BeginIngest(ctx context.Context, sourceKind, sourceDesc string) (IngestRun, error) {
	if err := ctx.Err(); err != nil {
		return IngestRun{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return IngestRun{}, fmt.Errorf("allocating ingest id: %w", err)
	}
	record := metadataIngest{
		Type: metadataIngestType, ID: id, StartedAt: nowRFC3339(),
		SourceKind: sourceKind, SourceDesc: sourceDesc,
	}
	if err := validateIngestRecord(record); err != nil {
		return IngestRun{}, fmt.Errorf("validating ingest start: %w", err)
	}
	return IngestRun{record: record}, nil
}

// ensureIngestRunTx publishes run once and rejects any identity collision with
// different immutable fields. The returned boolean reports whether this
// transaction inserted the record.
func ensureIngestRunTx(ctx context.Context, tx *sql.Tx, run IngestRun) (bool, error) {
	if err := validateIngestRecord(run.record); err != nil {
		return false, fmt.Errorf("validating ingest run: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO ingests (id, started_at, source_kind, source_desc)
		 VALUES (?, ?, ?, ?)`,
		run.record.ID, run.record.StartedAt, run.record.SourceKind, run.record.SourceDesc)
	if err != nil {
		return false, fmt.Errorf("recording ingest run: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking ingest run insertion: %w", err)
	}
	stored := metadataIngest{Type: metadataIngestType}
	if err := tx.QueryRowContext(ctx,
		`SELECT id,started_at,source_kind,source_desc FROM ingests WHERE id=?`,
		run.record.ID).Scan(
		&stored.ID, &stored.StartedAt, &stored.SourceKind, &stored.SourceDesc,
	); err != nil {
		return false, fmt.Errorf("reading ingest run %s: %w", run.record.ID, err)
	}
	if stored.ID != run.record.ID || stored.StartedAt != run.record.StartedAt ||
		stored.SourceKind != run.record.SourceKind || stored.SourceDesc != run.record.SourceDesc {
		return false, fmt.Errorf("ingest identity %s names different immutable metadata", run.record.ID)
	}
	return inserted == 1, nil
}

// resolveIngestNameTx applies the import idempotency rule: a live suffix
// candidate of name under parentID that carries blobHash AND was originally
// imported under this same basename means the file is already imported
// (skip). Content alone is not enough: a real source file named like an
// auto-suffixed copy ("report (2).pdf" next to an identical "report.pdf")
// is a distinct file, not a re-import. Otherwise returns the smallest free
// candidate name.
func resolveIngestNameTx(tx *sql.Tx, parentID int64, name, blobHash string) (string, int64, bool, error) {
	base, ext := splitSuffix(name)
	rows, err := tx.Query(
		`SELECT n.id, n.name, cv.blob_hash FROM nodes AS n
		 JOIN content_versions AS cv ON cv.version_id = n.current_version_id
		 WHERE n.parent_id = ? AND n.trashed_at IS NULL AND n.kind = 'file'`, parentID)
	if err != nil {
		return "", 0, false, fmt.Errorf("listing siblings for %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	type hashCandidate struct {
		nodeID       int64
		inNameFamily bool
	}
	var sameHash []hashCandidate
	taken := map[int]bool{}
	for rows.Next() {
		var sibID int64
		var sibName, sibHash string
		if err := rows.Scan(&sibID, &sibName, &sibHash); err != nil {
			return "", 0, false, fmt.Errorf("scanning sibling: %w", err)
		}
		n, inNameFamily := parseSuffix(sibName, base, ext)
		if sibHash == blobHash {
			sameHash = append(sameHash, hashCandidate{nodeID: sibID, inNameFamily: inNameFamily})
		}
		if !inNameFamily {
			continue
		}
		taken[n] = true
	}
	if err := rows.Err(); err != nil {
		return "", 0, false, fmt.Errorf("listing siblings for %q: %w", name, err)
	}
	for _, candidate := range sameHash {
		imported, err := sameOriginTx(tx, candidate.nodeID, name, candidate.inNameFamily)
		if err != nil {
			return "", 0, false, err
		}
		if imported {
			return "", candidate.nodeID, true, nil // already imported (possibly under a suffix)
		}
	}
	// Directories can occupy candidate names too; they don't carry content,
	// but their names are still taken. Probe them via the unique index by
	// walking ordinals and consulting taken plus a dir-name check.
	n := 1
	for {
		if !taken[n] {
			candidate := suffixedName(base, ext, n)
			var one int
			err := tx.QueryRow(
				`SELECT 1 FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`,
				parentID, candidate).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return candidate, 0, false, nil
			}
			if err != nil {
				return "", 0, false, fmt.Errorf("probing name %q: %w", candidate, err)
			}
		}
		n++
	}
}

// sameOriginTx reports whether node nodeID's active provenance leaf has a
// source basename (normalized) equal to name — i.e. the incoming file is a
// re-import of the same logical file, not a distinct source that merely
// shares content. A node with no provenance matches only when its virtual name
// belongs to the incoming suffix family: its origin is unknown, so the legacy
// idempotent fallback must not suppress an unrelated same-content file.
func sameOriginTx(tx *sql.Tx, nodeID int64, name string, allowUnknown bool) (bool, error) {
	rows, err := tx.Query(`
		SELECT p.original_path
		FROM provenance AS p
		WHERE p.node_id = ?
		  AND NOT EXISTS (
			SELECT 1 FROM provenance AS successor WHERE successor.supersedes = p.identity
		  )`, nodeID)
	if err != nil {
		return false, fmt.Errorf("reading provenance of node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()

	sawProvenance := false
	match := false
	for rows.Next() {
		var origPath string
		if err := rows.Scan(&origPath); err != nil {
			return false, fmt.Errorf("scanning provenance of node %d: %w", nodeID, err)
		}
		sawProvenance = true
		origName, err := NormalizeName(filepath.Base(origPath))
		if err != nil {
			continue // unnormalizable origin can't match a normalized name
		}
		if origName == name {
			match = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading provenance of node %d: %w", nodeID, err)
	}
	return match || (!sawProvenance && allowUnknown), nil
}

// IngestFile imports one already-durable blob as a node under parentID,
// applying the idempotency rule and recording provenance. Returns
// added=false when the content is already present under a candidate name.
func (s *Store) IngestFile(ctx context.Context, run IngestRun, parentID int64, name, blobHash string, size int64, mimeType, originalPath, originalMtime string) (Node, bool, error) {
	return s.ingestFile(ctx, run, parentID, name, blobHash, size, mimeType,
		originalPath, originalMtime, false)
}

// IngestFileExact imports one already-durable blob under exactly name. Unlike
// bulk migration it never suffixes or adopts an existing same-content node;
// a watched source needs its configured source identity to remain one-to-one.
func (s *Store) IngestFileExact(ctx context.Context, run IngestRun, parentID int64, name, blobHash string, size int64, mimeType, originalPath, originalMtime string) (Node, error) {
	node, _, err := s.ingestFile(ctx, run, parentID, name, blobHash, size, mimeType,
		originalPath, originalMtime, true)
	return node, err
}

func (s *Store) ingestFile(
	ctx context.Context, run IngestRun, parentID int64, name, blobHash string,
	size int64, mimeType, originalPath, originalMtime string, exact bool,
) (Node, bool, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, false, err
	}
	if err := validateIngestRecord(run.record); err != nil {
		return Node{}, false, fmt.Errorf("validating ingest run: %w", err)
	}
	var recordedMtime *string
	if originalMtime != "" {
		recordedMtime = &originalMtime
	}
	provenance := metadataProvenance{
		Type: metadataProvenanceType, NodeID: 1, IngestID: run.record.ID,
		OriginalPath: originalPath, OriginalMTime: recordedMtime,
	}
	if err := validateProvenanceFields(provenance); err != nil {
		return Node{}, false, fmt.Errorf("validating ingest provenance: %w", err)
	}
	var (
		created Node
		added   bool
	)
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		finalName := name
		if exact {
			var existingID int64
			err := tx.QueryRow(
				`SELECT id FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`,
				parentID, name).Scan(&existingID)
			switch {
			case err == nil:
				return fmt.Errorf("creating exact ingest %q under node %d: %w",
					name, parentID, ErrExists)
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("checking exact ingest name %q: %w", name, err)
			}
		} else {
			var existingID int64
			var skip bool
			finalName, existingID, skip, err = resolveIngestNameTx(tx, parentID, name, blobHash)
			if err != nil {
				return err
			}
			if skip {
				created, err = scanNode(tx.QueryRow(
					`SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id = ?`, existingID))
				if err != nil {
					return fmt.Errorf("reading idempotent ingest node %d: %w", existingID, err)
				}
				return nil
			}
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		var (
			authority auditAuthorityState
			scopes    []auditScopeState
			prior     Node
		)
		if active {
			prior, err = liveDirTx(tx, parentID)
			if err != nil {
				return err
			}
			authority, scopes, _, err = loadAuditedNodeAuthority(ctx, tx, parentID)
			if err != nil {
				return err
			}
		}
		ingestAdded, err := ensureIngestRunTx(ctx, tx, run)
		if err != nil {
			return err
		}
		operation, err := newContentVersionOperation()
		if err != nil {
			return err
		}
		var version ContentVersion
		created, version, err = s.createFileWithOperationTx(
			tx, parentID, finalName, blobHash, size, mimeType, operation,
		)
		if err != nil {
			return err
		}
		provenance.NodeID = created.ID
		provenance.Identity, err = provenanceIdentity(provenance)
		if err != nil {
			return fmt.Errorf("identifying provenance for %q: %w", finalName, err)
		}
		if err := validateProvenanceRecord(provenance); err != nil {
			return fmt.Errorf("validating provenance for %q: %w", finalName, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO provenance (
				identity, node_id, ingest_id, original_path, original_mtime, supersedes
			 ) VALUES (?, ?, ?, ?, ?, ?)`,
			provenance.Identity, provenance.NodeID, provenance.IngestID,
			provenance.OriginalPath, provenance.OriginalMTime, provenance.Supersedes); err != nil {
			return fmt.Errorf("recording provenance for %q: %w", finalName, err)
		}
		if run.record.SourceKind == "watch" {
			if err := insertWatchSourceTx(
				tx, run.record.SourceDesc, provenance.OriginalPath,
				created.ID, blobHash, size,
			); err != nil {
				return err
			}
		}
		if active {
			resultingParent, err := nodeByIDTx(tx, parentID)
			if err != nil {
				return err
			}
			metadata, err := makeAuditedIngestCreationMetadata(
				run.record, provenance, ingestAdded, operation.operationID,
			)
			if err != nil {
				return err
			}
			if err := persistAuditedNodeCreation(
				ctx, tx, s.vaultID, authority, scopes, prior, resultingParent,
				created, version, operation.operationID, operation.recordedAt, &metadata,
			); err != nil {
				return err
			}
		}
		added = true
		return nil
	})
	if err != nil {
		return Node{}, false, err
	}
	return created, added, nil
}

func insertWatchSourceTx(
	tx *sql.Tx, watchName, sourceRef string, nodeID int64, blobHash string, size int64,
) error {
	record := metadataWatchSource{
		Type: metadataWatchSourceType, WatchName: watchName, SourceRef: sourceRef,
		NodeID: nodeID, BlobHash: blobHash, Size: size,
	}
	if err := validateWatchSourceRecord(record); err != nil {
		return fmt.Errorf("validating watched source %q/%q: %w", watchName, sourceRef, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO watch_sources(watch_name,source_ref,node_id,blob_hash,size)
		 VALUES(?,?,?,?,?)`,
		watchName, sourceRef, nodeID, blobHash, size,
	); err != nil {
		return fmt.Errorf("recording watched source %q/%q: %w", watchName, sourceRef, err)
	}
	return nil
}
