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

// ValidatePushDestination checks the absolute virtual directory that receives
// a new source's node.
func ValidatePushDestination(parentPath string) error {
	_, err := pushParentSegments(parentPath)
	return err
}

// ValidatePushRunID checks the UUIDv4 that names one client scan.
func ValidatePushRunID(runID string) error {
	if err := validateUUIDv4(runID); err != nil {
		return fmt.Errorf("push run ID: %w", err)
	}
	return nil
}

// ValidatePushRef checks a canonical source-relative slash path.
func ValidatePushRef(ref string) error {
	if !utf8.ValidString(ref) || len(ref) > 4096 || ref == "" || ref == "." || path.IsAbs(ref) ||
		ref == ".." || strings.HasPrefix(ref, "../") || path.Clean(ref) != ref {
		return errors.New("push source reference must be a canonical relative slash path of at most 4096 bytes")
	}
	for part := range strings.SplitSeq(ref, "/") {
		if _, err := NormalizeName(part); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePushSource checks a source identity and its modification time.
func ValidatePushSource(source PushSource) error {
	if err := ValidatePushName(source.Name); err != nil {
		return err
	}
	if err := ValidatePushRef(source.Ref); err != nil {
		return err
	}
	if len(source.ModifiedAt) > 64 {
		return errors.New("push modification time exceeds 64 bytes")
	}
	if source.ModifiedAt != "" {
		if err := validateProvenanceTime(source.ModifiedAt); err != nil {
			return err
		}
	}
	return nil
}

// pushState reports the last accepted source bytes, independently of the
// node's current version. Known sources in trash are errors, never new imports.
type pushState struct {
	Node       Node
	Hash       string
	Size       int64
	AcceptedAt string
}

func pushStateTx(ctx context.Context, tx *sql.Tx, name, ref string) (pushState, error) {
	var state pushState
	err := tx.QueryRowContext(ctx, `SELECT node_id, blob_hash, size, accepted_at
  FROM push_sources WHERE push_name=? AND source_ref=?`, name, ref).
		Scan(&state.Node.ID, &state.Hash, &state.Size, &state.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// A stopped daemon watch can hand this identity to a client without
		// uploading unchanged bytes. A push cursor takes precedence once a push
		// has accepted a changed source observation.
		cursor, watchErr := watchSourceTx(ctx, tx, name, ref)
		if watchErr != nil {
			return pushState{}, watchErr
		}
		state.Node.ID, state.Hash, state.Size = cursor.nodeID, cursor.blobHash, cursor.size
	} else if err != nil {
		return pushState{}, fmt.Errorf("reading push source: %w", err)
	}
	// Use the snapshot's node projection so the source and head agree.
	state.Node, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id=?`, state.Node.ID))
	if err != nil {
		return pushState{}, err
	}
	if state.Node.TrashedAt != nil {
		return pushState{}, fmt.Errorf("push source %q/%q maps to trashed node %d: %w", name, ref, state.Node.ID, ErrExists)
	}
	if _, err := requirePhysicalAuthorityTx(tx, state.Node.BlobHash); err != nil {
		return pushState{}, err
	}
	return state, nil
}

// PushCursor is the last accepted bytes for one push source. A client that
// holds equal bytes needs no upload. Trashed reports that the source's node is
// in trash, which push treats as an error rather than a new source.
type PushCursor struct {
	Ref     string
	NodeID  int64
	Hash    string
	Size    int64
	Trashed bool
}

// MaxPushCursorPage bounds one page of PushCursors.
const MaxPushCursorPage = 1000

// PushCursors lists the cursors of one push name in source-reference byte
// order, after the reference after. Before a push accepts a source, the
// cursor of a daemon watch of the same name and reference stands in for it.
func (s *Store) PushCursors(ctx context.Context, name, after string, limit int) ([]PushCursor, error) {
	if err := ValidatePushName(name); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPushCursorPage {
		return nil, fmt.Errorf("push cursor page limit must be between 1 and %d", MaxPushCursorPage)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.source_ref, p.node_id, p.blob_hash, p.size, n.trashed_at IS NOT NULL
		FROM push_sources p JOIN nodes n ON n.id=p.node_id
		WHERE p.push_name=? AND p.source_ref>?
		UNION ALL
		SELECT w.source_ref, w.node_id, w.blob_hash, w.size, n.trashed_at IS NOT NULL
		FROM watch_sources w JOIN nodes n ON n.id=w.node_id
		WHERE w.watch_name=? AND w.source_ref>? AND NOT EXISTS (
			SELECT 1 FROM push_sources p WHERE p.push_name=w.watch_name AND p.source_ref=w.source_ref)
		ORDER BY 1 LIMIT ?`, name, after, name, after, limit)
	if err != nil {
		return nil, fmt.Errorf("listing push cursors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	cursors := make([]PushCursor, 0, limit)
	for rows.Next() {
		var cursor PushCursor
		if err := rows.Scan(&cursor.Ref, &cursor.NodeID, &cursor.Hash, &cursor.Size, &cursor.Trashed); err != nil {
			return nil, fmt.Errorf("reading push cursor: %w", err)
		}
		cursors = append(cursors, cursor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing push cursors: %w", err)
	}
	return cursors, nil
}

// PushContent is one verified upload for a push source. RunID names the
// client's scan, so every file one scan accepts joins one ingest run and one
// collection. ParentPath and Name place a newly created node; an existing
// source keeps its node wherever it is.
type PushContent struct {
	RunID      string
	ParentPath string
	Name       string
	Hash       string
	Size       int64
	MIMEType   string
}

// AcceptPush records verified bytes and their source atomically. A new source
// always gets its own node. Missing destination directories are created in
// the same transaction, and only when a new node is created.
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
	if err := ValidatePushRunID(content.RunID); err != nil {
		return Node{}, "", err
	}
	var node Node
	var outcome string
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		run, err := pushRunTx(ctx, tx, content.RunID, source.Name)
		if err != nil {
			return err
		}
		state, stateErr := pushStateTx(ctx, tx, source.Name, source.Ref)
		switch {
		case errors.Is(stateErr, ErrNotFound):
			plan := IngestDirectoryPlan{anchorID: s.rootID, segments: segments}
			receipt, _, _, createErr := s.ingestFileTx(ctx, tx, run, 0, content.Name,
				content.Hash, content.Size, content.MIMEType, source.Ref, source.ModifiedAt,
				ingestFileOptions{exact: true, directoryPlan: &plan}, physical...)
			node, outcome = receipt.Node, "added"
			return createErr
		case stateErr != nil:
			return stateErr
		case state.Hash == content.Hash && state.Size == content.Size:
			node, outcome = state.Node, "skipped"
			return nil
		}
		run, err = pushObservationRun(run, state.AcceptedAt)
		if err != nil {
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

// pushRunTx joins the client's ingest run for one scan, creating it with the
// first document. A run ID that names another source's run is refused.
func pushRunTx(ctx context.Context, tx *sql.Tx, runID, name string) (IngestRun, error) {
	record := metadataIngest{Type: metadataIngestType}
	err := tx.QueryRowContext(ctx, `SELECT id,started_at,source_kind,source_desc FROM ingests WHERE id=?`,
		runID).Scan(&record.ID, &record.StartedAt, &record.SourceKind, &record.SourceDesc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		record = metadataIngest{
			Type: metadataIngestType, ID: runID, StartedAt: nowRFC3339(),
			SourceKind: "push", SourceDesc: name,
		}
	case err != nil:
		return IngestRun{}, fmt.Errorf("reading push run %s: %w", runID, err)
	case record.SourceKind != "push" || record.SourceDesc != name:
		return IngestRun{}, fmt.Errorf("push run %s belongs to another source: %w", runID, ErrExists)
	}
	if err := validateIngestRecord(record); err != nil {
		return IngestRun{}, fmt.Errorf("validating push run: %w", err)
	}
	return IngestRun{record: record}, nil
}

// pushObservationRun keeps each source's observations strictly ordered, which
// metadata restore validates. A shared run normally starts after the
// source's previous observation. After the wall clock steps backward, or when
// one run observes a source twice, the observation gets a separate run that
// starts one nanosecond after the previous one.
func pushObservationRun(run IngestRun, previousAcceptedAt string) (IngestRun, error) {
	if previousAcceptedAt == "" {
		return run, nil
	}
	previous, err := time.Parse(time.RFC3339Nano, previousAcceptedAt)
	if err != nil {
		return IngestRun{}, fmt.Errorf("reading push observation time: %w", err)
	}
	current, err := time.Parse(time.RFC3339Nano, run.record.StartedAt)
	if err != nil {
		return IngestRun{}, fmt.Errorf("reading current push time: %w", err)
	}
	if current.After(previous) {
		return run, nil
	}
	id, err := newUUIDv4()
	if err != nil {
		return IngestRun{}, fmt.Errorf("allocating push run: %w", err)
	}
	record := run.record
	record.ID = id
	record.StartedAt = previous.Add(time.Nanosecond).UTC().Format(timestampLayout)
	return IngestRun{record: record}, nil
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
	node, err = s.observeOperationalIngestTx(ctx, tx, run, node, fact, ingestAdded, node.CurrentVersionID)
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
// A source keeps one node for life, and no two sources share a node. Its
// independent cursor may outlive the exact content-version binding that
// supplied the accepted bytes.
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
		if err := ValidatePushSource(PushSource{Name: name, Ref: ref}); err != nil {
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
	claimed := make(map[int64]bool)
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
		if claimed[record.NodeID] {
			return fmt.Errorf("push source cursor %q/%q shares node %d with another source", record.PushName, record.SourceRef, record.NodeID)
		}
		claimed[record.NodeID] = true
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
	if err := ValidatePushSource(PushSource{Name: record.PushName, Ref: record.SourceRef}); err != nil {
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
