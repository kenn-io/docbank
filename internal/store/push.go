package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
)

// PushSource identifies one portable client-owned source. Its cursor is keyed
// by push name and relative path; no client-side state or daemon filesystem
// path is involved.
type PushSource struct {
	Name       string
	Ref        string
	ModifiedAt string
	Duplicates string
}

// ValidatePushName checks a portable push name.
func ValidatePushName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("push name must contain 1-64 characters")
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || strings.ContainsRune("-_.", char) {
			continue
		}
		return errors.New("push name permits lowercase letters, digits, hyphens, underscores, and dots")
	}
	return nil
}

// ValidatePushDuplicates checks the new-source duplicate policy.
func ValidatePushDuplicates(policy string) error {
	switch policy {
	case "link", "skip", "create":
		return nil
	default:
		return errors.New("push duplicates must be link, skip, or create")
	}
}

// ValidatePushDestination checks the absolute virtual directory that receives
// a new source's node.
func ValidatePushDestination(parentPath string) error {
	_, err := pushParentSegments(parentPath)
	return err
}

// ValidatePushSource checks the identity and new-source duplicate policy.
func ValidatePushSource(source PushSource) error {
	if err := ValidatePushName(source.Name); err != nil {
		return err
	}
	if !utf8.ValidString(source.Ref) || len(source.Ref) > 4096 || source.Ref == "" || source.Ref == "." || path.IsAbs(source.Ref) || source.Ref == ".." || strings.HasPrefix(source.Ref, "../") || path.Clean(source.Ref) != source.Ref {
		return errors.New("push source reference must be a canonical relative slash path of at most 4096 bytes")
	}
	for part := range strings.SplitSeq(source.Ref, "/") {
		if _, err := NormalizeName(part); err != nil {
			return err
		}
	}
	if len(source.ModifiedAt) > 64 {
		return errors.New("push modification time exceeds 64 bytes")
	}
	if source.ModifiedAt != "" {
		if err := validateProvenanceTime(source.ModifiedAt); err != nil {
			return err
		}
	}
	return ValidatePushDuplicates(source.Duplicates)
}

// PushState reports the last accepted source bytes, independently of the
// node's current version. Known sources in trash are errors, never new imports.
type PushState struct {
	Node       Node
	Hash       string
	Size       int64
	AcceptedAt string
}

func pushStateTx(ctx context.Context, tx *sql.Tx, name, ref string) (PushState, error) {
	var state PushState
	err := tx.QueryRowContext(ctx, `SELECT node_id, blob_hash, size, accepted_at
  FROM push_sources WHERE push_name=? AND source_ref=?`, name, ref).
		Scan(&state.Node.ID, &state.Hash, &state.Size, &state.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// A stopped daemon watch can hand this identity to a client without
		// uploading unchanged bytes. A push cursor takes precedence once a push
		// has accepted a changed source observation.
		cursor, watchErr := watchSourceTx(ctx, tx, name, ref)
		if watchErr != nil {
			return PushState{}, watchErr
		}
		state.Node.ID, state.Hash, state.Size = cursor.nodeID, cursor.blobHash, cursor.size
	} else if err != nil {
		return PushState{}, fmt.Errorf("reading push source: %w", err)
	}
	// Use the snapshot's node projection so the source and head agree.
	state.Node, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id=?`, state.Node.ID))
	if err != nil {
		return PushState{}, err
	}
	if state.Node.TrashedAt != nil {
		return PushState{}, fmt.Errorf("push source %q/%q maps to trashed node %d: %w", name, ref, state.Node.ID, ErrExists)
	}
	if _, err := requirePhysicalAuthorityTx(tx, state.Node.BlobHash); err != nil {
		return PushState{}, err
	}
	return state, nil
}

// PushSourceState reads a source cursor and its live node in one snapshot.
func (s *Store) PushSourceState(ctx context.Context, source PushSource) (PushState, error) {
	if err := ValidatePushSource(source); err != nil {
		return PushState{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PushState{}, fmt.Errorf("opening push snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := pushStateTx(ctx, tx, source.Name, source.Ref)
	if err != nil {
		return PushState{}, err
	}
	if err := tx.Commit(); err != nil {
		return PushState{}, fmt.Errorf("closing push snapshot: %w", err)
	}
	return state, nil
}

// PushContent is one verified upload for a push source. ParentPath and Name
// place a newly created node; an existing source keeps its node wherever it is.
type PushContent struct {
	ParentPath string
	Name       string
	Hash       string
	Size       int64
	MIMEType   string
}

// AcceptPush records verified bytes and their source atomically. A new source
// may link to a node this push name already owns; linked sources share that
// node, including its future versions. Missing destination directories are
// created in the same transaction, and only when a new node is created.
func (s *Store) AcceptPush(
	ctx context.Context, source PushSource, content PushContent, physical ...BlobPhysical,
) (Node, string, error) {
	if err := ValidatePushSource(source); err != nil {
		return Node{}, "", err
	}
	segments, err := pushParentSegments(content.ParentPath)
	if err != nil {
		return Node{}, "", err
	}
	run, err := s.BeginIngest(ctx, "push", source.Name)
	if err != nil {
		return Node{}, "", err
	}
	var node Node
	var outcome string
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		state, stateErr := pushStateTx(ctx, tx, source.Name, source.Ref)
		switch {
		case stateErr == nil:
			if state.Hash == content.Hash && state.Size == content.Size {
				node, outcome = state.Node, "skipped"
				return nil
			}
			if err := advancePushObservationTime(&run, state.AcceptedAt); err != nil {
				return err
			}
			node, outcome = state.Node, "skipped"
			if node.BlobHash != content.Hash || node.Size != content.Size {
				node, _, err = s.replaceContentTx(ctx, tx, node, UnconditionalRev,
					content.Hash, content.Size, content.MIMEType, physical...)
				if err != nil {
					return err
				}
				outcome = "updated"
			}
		case !errors.Is(stateErr, ErrNotFound):
			return stateErr
		default:
			duplicate, found, findErr := s.pushDuplicateTx(ctx, tx, source, content, physical...)
			if findErr != nil {
				return findErr
			}
			if !found {
				plan := IngestDirectoryPlan{anchorID: s.rootID, segments: segments}
				receipt, _, _, createErr := s.ingestFileTx(ctx, tx, run, 0, content.Name,
					content.Hash, content.Size, content.MIMEType, source.Ref, source.ModifiedAt,
					ingestFileOptions{exact: true, directoryPlan: &plan}, physical...)
				node, outcome = receipt.Node, "added"
				return createErr
			}
			node = duplicate
			if source.Duplicates == "skip" {
				outcome = "duplicate_skipped"
				return nil
			}
			outcome = "linked"
		}
		node, err = s.recordPushObservationTx(ctx, tx, run, source, node, content)
		return err
	})
	if err != nil {
		return Node{}, "", fmt.Errorf("accepting push source %q/%q: %w", source.Name, source.Ref, err)
	}
	return node, outcome, nil
}

func pushParentSegments(parentPath string) ([]string, error) {
	if !strings.HasPrefix(parentPath, "/") {
		return nil, fmt.Errorf("push destination %q must be an absolute virtual path", parentPath)
	}
	return normalizeIngestDirectorySegments(parentPath, splitPath(parentPath))
}

// advancePushObservationTime keeps per-source observation order across a
// wall-clock rollback and portable metadata restore. The caller holds the
// write transaction.
func advancePushObservationTime(run *IngestRun, previousAcceptedAt string) error {
	if previousAcceptedAt == "" {
		return nil
	}
	previous, err := time.Parse(time.RFC3339Nano, previousAcceptedAt)
	if err != nil {
		return fmt.Errorf("reading push observation time: %w", err)
	}
	current, err := time.Parse(time.RFC3339Nano, run.record.StartedAt)
	if err != nil {
		return fmt.Errorf("reading current push time: %w", err)
	}
	if !current.After(previous) {
		run.record.StartedAt = previous.Add(time.Nanosecond).UTC().Format(timestampLayout)
	}
	return nil
}

// pushDuplicateTx finds a live node with the same current bytes among the
// nodes this push name already owns, including nodes adopted from a watch of
// the same name. Unrelated vault documents are never duplicate targets, so a
// push cannot version a document that another source created.
func (s *Store) pushDuplicateTx(
	ctx context.Context, tx *sql.Tx, source PushSource, content PushContent,
	physical ...BlobPhysical,
) (Node, bool, error) {
	if source.Duplicates == "create" {
		return Node{}, false, nil
	}
	duplicate, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM `+nodeFrom+`
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.blob_hash=?
		  AND n.id IN (
			SELECT node_id FROM push_sources WHERE push_name=?
			UNION SELECT node_id FROM watch_sources WHERE watch_name=?
		  )
		ORDER BY n.id LIMIT 1`, content.Hash, source.Name, source.Name))
	if errors.Is(err, ErrNotFound) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, err
	}
	if duplicate.Size != content.Size {
		return Node{}, false, errors.New("duplicate size does not match verified bytes")
	}
	if err := s.EnsureBlobTx(tx, content.Hash, content.Size, physical...); err != nil {
		return Node{}, false, err
	}
	return duplicate, true, nil
}

func (s *Store) recordPushObservationTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, source PushSource, node Node,
	content PushContent,
) (Node, error) {
	ingestAdded, err := s.ensureIngestRunForMutationTx(ctx, tx, run)
	if err != nil {
		return Node{}, err
	}
	fact := metadataProvenance{
		Type: metadataProvenanceType, NodeID: node.ID, IngestID: run.ID(), OriginalPath: source.Ref,
	}
	if source.ModifiedAt != "" {
		fact.OriginalMTime = &source.ModifiedAt
	}
	fact.Identity, err = provenanceIdentity(fact)
	if err != nil {
		return Node{}, err
	}
	node, err = s.observeOperationalIngestTx(ctx, tx, run, node, fact, ingestAdded)
	if err != nil {
		return Node{}, err
	}
	err = insertPushSourceTx(ctx, tx, source, node.ID, fact.Identity,
		content.Hash, content.Size, run.record.StartedAt)
	return node, err
}

func insertPushSourceTx(
	ctx context.Context, tx *sql.Tx, source PushSource, nodeID int64,
	provenanceIdentity, blobHash string, size int64, acceptedAt string,
) error {
	record := metadataPushSource{
		Type: metadataPushSourceType, PushName: source.Name, SourceRef: source.Ref,
		NodeID: nodeID, ProvenanceIdentity: provenanceIdentity,
		BlobHash: blobHash, Size: size, AcceptedAt: acceptedAt,
	}
	if err := validatePushSourceRecord(record); err != nil {
		return fmt.Errorf("validating pushed source %q/%q: %w", source.Name, source.Ref, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO push_sources(
		push_name,source_ref,node_id,provenance_identity,blob_hash,size,accepted_at
	) VALUES(?,?,?,?,?,?,?)
	ON CONFLICT(push_name,source_ref) DO UPDATE SET
		node_id=excluded.node_id,
		provenance_identity=excluded.provenance_identity,
		blob_hash=excluded.blob_hash,
		size=excluded.size,
		accepted_at=excluded.accepted_at`,
		record.PushName, record.SourceRef, record.NodeID, record.ProvenanceIdentity,
		record.BlobHash, record.Size, record.AcceptedAt); err != nil {
		return fmt.Errorf("recording pushed source %q/%q: %w", source.Name, source.Ref, err)
	}
	return nil
}

// validatePushSourceRelations rejects ambiguous portable cursor authority.
// A source keeps one node for life, while several source identities may
// deliberately share that node. Its independent cursor may outlive the exact
// content-version binding that supplied the accepted bytes.
func validatePushSourceRelations(ctx context.Context, tx metadataQuerier) (retErr error) {
	type pushFact struct {
		identity   string
		nodeID     int64
		acceptedAt string
		bindings   int64
		blobHash   string
		size       int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.source_desc,p.original_path,p.node_id,p.identity,i.started_at,
  COUNT(b.content_version_id),MAX(cv.blob_hash),MAX(cv.size)
  FROM ingests i JOIN provenance p ON p.ingest_id=i.id
  LEFT JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
  LEFT JOIN content_versions cv ON cv.version_id=b.content_version_id
  WHERE i.source_kind='push'
  GROUP BY i.source_desc,p.original_path,p.node_id,p.identity,i.started_at
  ORDER BY i.source_desc,p.original_path,i.started_at,i.id`)
	if err != nil {
		return fmt.Errorf("reading push provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	latest := make(map[pushSourceKey]pushFact)
	times := make(map[pushSourceKey]map[string]bool)
	for rows.Next() {
		var name, ref, identity, stamp string
		var nodeID int64
		var bindingCount int64
		var blobHash sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&name, &ref, &nodeID, &identity, &stamp,
			&bindingCount, &blobHash, &size); err != nil {
			return fmt.Errorf("reading push fact: %w", err)
		}
		if err := ValidatePushSource(PushSource{Name: name, Ref: ref, Duplicates: "link"}); err != nil {
			return err
		}
		if bindingCount > 1 {
			return errors.New("push fact binds more than one content version")
		}
		if bindingCount == 1 && (!blobHash.Valid || !size.Valid) {
			return errors.New("push fact has an invalid content-version binding")
		}
		key := pushSourceKey{pushName: name, sourceRef: ref}
		if prior, exists := latest[key]; exists && prior.nodeID != nodeID {
			return errors.New("push source maps to more than one node")
		}
		if times[key] == nil {
			times[key] = make(map[string]bool)
		}
		if times[key][stamp] {
			return errors.New("push source observation times must be distinct")
		}
		times[key][stamp] = true
		fact := pushFact{identity: identity, nodeID: nodeID, acceptedAt: stamp, bindings: bindingCount}
		if blobHash.Valid {
			fact.blobHash = blobHash.String
		}
		if size.Valid {
			fact.size = size.Int64
		}
		latest[key] = fact
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading push provenance: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing push provenance: %w", err)
	}
	nodeKinds, err := loadMetadataNodeKinds(ctx, tx)
	if err != nil {
		return err
	}
	cursors, err := tx.QueryContext(ctx, `SELECT push_sources.push_name,push_sources.source_ref,
		push_sources.node_id,push_sources.provenance_identity,push_sources.blob_hash,
		push_sources.size,push_sources.accepted_at,catalog.size FROM push_sources
		LEFT JOIN blobs catalog ON catalog.hash=push_sources.blob_hash
		ORDER BY push_sources.push_name,push_sources.source_ref`)
	if err != nil {
		return fmt.Errorf("reading push source cursors: %w", err)
	}
	defer func() {
		if err := cursors.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing push source cursors: %w", err)
		}
	}()
	seen := make(map[pushSourceKey]bool)
	for cursors.Next() {
		var record metadataPushSource
		var catalogSize sql.NullInt64
		record.Type = metadataPushSourceType
		if err := cursors.Scan(&record.PushName, &record.SourceRef, &record.NodeID,
			&record.ProvenanceIdentity, &record.BlobHash, &record.Size, &record.AcceptedAt,
			&catalogSize); err != nil {
			return fmt.Errorf("reading push source cursor: %w", err)
		}
		if err := validatePushSourceRecord(record); err != nil {
			return err
		}
		key := pushSourceKey{pushName: record.PushName, sourceRef: record.SourceRef}
		fact, exists := latest[key]
		if !exists || seen[key] || fact.identity != record.ProvenanceIdentity ||
			fact.nodeID != record.NodeID || fact.acceptedAt != record.AcceptedAt {
			return fmt.Errorf("push source cursor %q/%q does not match its latest provenance", record.PushName, record.SourceRef)
		}
		if nodeKinds[record.NodeID] != nodeKindFile {
			return fmt.Errorf("push source cursor %q/%q references non-file node %d", record.PushName, record.SourceRef, record.NodeID)
		}
		if fact.bindings == 1 && (fact.blobHash != record.BlobHash || fact.size != record.Size) {
			return fmt.Errorf("push source cursor %q/%q differs from its bound content version", record.PushName, record.SourceRef)
		}
		if catalogSize.Valid && catalogSize.Int64 != record.Size {
			return fmt.Errorf("push source cursor %q/%q size differs from blob catalog", record.PushName, record.SourceRef)
		}
		seen[key] = true
	}
	if err := cursors.Err(); err != nil {
		return fmt.Errorf("reading push source cursors: %w", err)
	}
	if len(seen) != len(latest) {
		return errors.New("push provenance lacks an independent source cursor")
	}
	return nil
}

type pushSourceKey struct {
	pushName  string
	sourceRef string
}

func validatePushSourceRecord(record metadataPushSource) error {
	if record.Type != metadataPushSourceType || record.NodeID <= 0 || record.Size < 0 {
		return errors.New("invalid push source cursor")
	}
	if err := ValidatePushSource(PushSource{
		Name: record.PushName, Ref: record.SourceRef, Duplicates: "link",
	}); err != nil {
		return err
	}
	if _, err := packstore.ParseHash(record.ProvenanceIdentity); err != nil {
		return fmt.Errorf("invalid push source provenance identity: %w", err)
	}
	if _, err := packstore.ParseHash(record.BlobHash); err != nil {
		return fmt.Errorf("invalid push source blob hash: %w", err)
	}
	return validateMetadataTime("push source accepted_at", record.AcceptedAt)
}
