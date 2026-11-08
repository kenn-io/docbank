package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"math"
	"path"
	"slices"
)

var (
	ErrPublicationScanSuperseded    = errors.New("publication scan superseded")
	ErrPublicationScanSealed        = errors.New("publication scan sealed")
	ErrPublicationSourceStale       = errors.New("publication source stale")
	ErrPublicationOperationMismatch = errors.New("publication operation payload mismatch")
	ErrPublishedSourceDiverged      = errors.New("published source archive head diverged")
	ErrNoteSourceUnavailable        = errors.New("published note source unavailable")
	ErrInvalidNotePublication       = errors.New("invalid note publication request")
)

type NotePublicationBeginRequest struct{ Name, Destination, PolicyFingerprint, OperationID string }
type NotePublicationSourcesRequest struct {
	Name, ScanID, After string
	Limit               int
}
type NotePublicationSourcesPage struct {
	Scan  NotePublicationScan
	Items []NotePublicationSource
	Next  string
}
type NotePublicationUploadRequest struct {
	Name, ScanID, SourceRef, FileName, ParentPath, ModifiedAt string
	OperationID, Hash                                         string
	Size, ExpectedSourceRevision                              int64
}
type NotePublicationReceipt struct {
	Scan    NotePublicationScan
	Source  NotePublicationSource
	Outcome string
}
type NotePublicationSeenRequest struct {
	Name, ScanID string
	Bindings     []PublishedNoteIdentity
}
type NotePublicationAck struct {
	Scan    NotePublicationScan
	Sources []PublishedNoteIdentity
}
type NotePublicationCompleteRequest struct {
	Name, ScanID, PolicyFingerprint string
	InventoryComplete               bool
}

// Operational scans and receipts never enter portable metadata. Their random
// database epoch makes an old process stale even if a restore repeats revisions.
type noteScanState struct {
	NotePublicationScan

	Name, Destination, CompletedAt string
	Sealed                         bool
}

func publicationRequestDigest(value any) string {
	encoded, err := json.Marshal(value) // callers pass only fixed structs
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func readNoteScanTx(ctx context.Context, q metadataQuerier, name, scanID string, allowSealed bool) (noteScanState, error) {
	var scan noteScanState
	err := q.QueryRowContext(ctx, `SELECT s.scan_id,s.database_epoch,s.policy_fingerprint,s.epoch,
		s.namespace_revision,p.name,p.destination,s.sealed,s.completed_at
		FROM note_publication_scans s JOIN note_publications p ON p.name=s.publication_name
		JOIN note_publication_state r ON r.singleton=1 AND r.database_epoch=s.database_epoch
		WHERE p.name=$1 AND s.scan_id=$2 AND p.active=1
		AND s.epoch=(SELECT MAX(x.epoch) FROM note_publication_scans x WHERE x.publication_name=p.name)`, name, scanID).
		Scan(&scan.ScanID, &scan.DatabaseEpoch, &scan.PolicyFingerprint, &scan.Epoch, &scan.NamespaceRevision,
			&scan.Name, &scan.Destination, &scan.Sealed, &scan.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return scan, ErrPublicationScanSuperseded
	}
	if err != nil {
		return scan, err
	}
	if scan.Sealed && !allowSealed {
		return scan, ErrPublicationScanSealed
	}
	return scan, nil
}

// BeginNotePublicationScan returns one scan for an operation UUID. Replaying a
// superseded begin never starts it again or displaces the winning publisher.
func (s *Store) BeginNotePublicationScan(ctx context.Context, request NotePublicationBeginRequest) (NotePublicationScan, error) {
	if err := ValidatePushName(request.Name); err != nil {
		return NotePublicationScan{}, err
	}
	if err := ValidatePushDestination(request.Destination); err != nil {
		return NotePublicationScan{}, err
	}
	if err := validateNotePolicyFingerprint(request.PolicyFingerprint); err != nil {
		return NotePublicationScan{}, err
	}
	if err := validateUUIDv4(request.OperationID); err != nil {
		return NotePublicationScan{}, err
	}
	digest := publicationRequestDigest(request)
	var result NotePublicationScan
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var priorID, priorDigest string
		err := tx.QueryRowContext(ctx, `SELECT scan_id,request_sha256 FROM note_publication_scans WHERE begin_operation_id=$1`, request.OperationID).Scan(&priorID, &priorDigest)
		if err == nil {
			if digest != priorDigest {
				return ErrPublicationOperationMismatch
			}
			scan, err := readNoteScanTx(ctx, tx, request.Name, priorID, true)
			result = scan.NotePublicationScan
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM note_publication_operations WHERE operation_id=$1)+(SELECT COUNT(*) FROM note_publication_recovery_operations WHERE operation_id=$1)`, request.OperationID).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return ErrPublicationOperationMismatch
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_publications(name,destination,policy_fingerprint,namespace_revision,active,last_complete_at,last_accepted_at)
			VALUES($1,$2,$3,1,1,'','') ON CONFLICT(name) DO UPDATE SET
			namespace_revision=CASE WHEN note_publications.destination<>excluded.destination OR note_publications.policy_fingerprint<>excluded.policy_fingerprint OR note_publications.active<>1
			THEN note_publications.namespace_revision+1 ELSE note_publications.namespace_revision END,
			destination=excluded.destination,policy_fingerprint=excluded.policy_fingerprint,active=1`, request.Name, request.Destination, request.PolicyFingerprint)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epoch),0) FROM note_publication_scans WHERE publication_name=$1`, request.Name).Scan(&result.Epoch); err != nil {
			return err
		}
		if result.Epoch == math.MaxInt64 {
			return errors.New("publication scan epoch exhausted")
		}
		result.Epoch++
		if _, err := tx.ExecContext(ctx, `DELETE FROM note_publication_seen WHERE scan_id IN
			(SELECT scan_id FROM note_publication_scans WHERE publication_name=$1)`, request.Name); err != nil {
			return err
		}
		result.ScanID, err = newUUIDv4()
		if err != nil {
			return err
		}
		result.PolicyFingerprint = request.PolicyFingerprint
		if err := tx.QueryRowContext(ctx, `SELECT namespace_revision,database_epoch FROM note_publications CROSS JOIN note_publication_state WHERE name=$1 AND singleton=1`, request.Name).Scan(&result.NamespaceRevision, &result.DatabaseEpoch); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_publication_scans(scan_id,publication_name,database_epoch,epoch,begin_operation_id,request_sha256,policy_fingerprint,namespace_revision,sealed,completed_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,'')`, result.ScanID, request.Name, result.DatabaseEpoch, result.Epoch, request.OperationID, digest, result.PolicyFingerprint, result.NamespaceRevision)
		return err
	})
	return result, err
}

const publishedNoteSourceColumns = `s.publication_name,s.source_ref,s.node_id,s.content_version_id,s.blob_hash,s.size,s.source_revision,
	p.namespace_revision,s.presence,s.availability,n.id,n.current_version_id,cv.blob_hash,cv.size,n.trashed_at,v.version_id,g.head_version_id,g.source_revision`
const publishedNoteSourceFrom = `published_note_sources s JOIN note_publications p ON p.name=s.publication_name
	LEFT JOIN nodes n ON n.id=s.node_id LEFT JOIN content_versions cv ON cv.version_id=n.current_version_id
	LEFT JOIN content_versions v ON v.version_id=s.content_version_id
 LEFT JOIN note_publication_recoveries g ON g.publication_name=s.publication_name AND g.source_ref=s.source_ref`

func scanPublishedNote(row scanner, vaultID string) (NotePublicationSource, error) {
	var source NotePublicationSource
	var nodeID, size, grantRevision sql.NullInt64
	var version, hash, trashed, retained, grantHead sql.NullString
	id := &source.Identity
	id.VaultID = vaultID
	err := row.Scan(&id.Publication, &id.SourceRef, &id.NodeID, &id.ContentVersionID, &id.Hash, &id.Size, &id.SourceRevision,
		&id.NamespaceRevision, &source.Presence, &source.Availability, &nodeID, &version, &hash, &size, &trashed, &retained, &grantHead, &grantRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return source, ErrNotFound
	}
	if err != nil {
		return source, err
	}
	if nodeID.Valid {
		source.ArchiveHead = &NoteArchiveHead{NodeID: nodeID.Int64, ContentVersionID: version.String, Hash: hash.String, Size: size.Int64}
	}
	if !nodeID.Valid || !retained.Valid || source.Availability == "unavailable" {
		source.Availability = "unavailable"
		source.Conflict = "unavailable"
	} else if trashed.Valid {
		source.Conflict = "trashed"
	} else if version.String != id.ContentVersionID {
		source.Conflict = "diverged"
	}
	if grantHead.Valid && grantHead.String == version.String && grantRevision.Int64 == id.SourceRevision && source.Conflict == "diverged" {
		source.Recovery = &NotePublicationRecoveryGrant{SourceRef: id.SourceRef, ExpectedHeadVersionID: grantHead.String, SourceRevision: grantRevision.Int64}
	}
	return source, nil
}

func (s *Store) publishedNoteSourceTx(ctx context.Context, q metadataQuerier, name, ref string) (NotePublicationSource, error) {
	return scanPublishedNote(q.QueryRowContext(ctx, `SELECT `+publishedNoteSourceColumns+` FROM `+publishedNoteSourceFrom+` WHERE s.publication_name=$1 AND s.source_ref=$2`, name, ref), s.VaultID())
}

func (s *Store) NotePublicationSources(ctx context.Context, request NotePublicationSourcesRequest) (NotePublicationSourcesPage, error) {
	var page NotePublicationSourcesPage
	if err := ValidatePushName(request.Name); err != nil {
		return page, err
	}
	if err := validateUUIDv4(request.ScanID); err != nil {
		return page, err
	}
	if request.After != "" {
		if err := ValidatePushRef(request.After); err != nil {
			return page, err
		}
	}
	if request.Limit < 1 || request.Limit > MaxPushCursorPage {
		return page, errors.New("publication cursor page limit must be between 1 and 1000")
	}
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		scan, err := readNoteScanTx(ctx, tx, request.Name, request.ScanID, true)
		if err != nil {
			return err
		}
		page.Scan = scan.NotePublicationScan
		rows, err := tx.QueryContext(ctx, `SELECT `+publishedNoteSourceColumns+` FROM `+publishedNoteSourceFrom+`
			WHERE s.publication_name=$1 AND s.source_ref>$2 ORDER BY s.source_ref LIMIT $3`, request.Name, request.After, request.Limit+1)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		page.Items = make([]NotePublicationSource, 0, request.Limit)
		for rows.Next() {
			source, err := scanPublishedNote(rows, s.VaultID())
			if err != nil {
				return err
			}
			if len(page.Items) == request.Limit {
				page.Next = page.Items[len(page.Items)-1].Identity.SourceRef
				break
			}
			page.Items = append(page.Items, source)
		}
		return rows.Err()
	})
	return page, err
}

func validateNoteUpload(request NotePublicationUploadRequest) error {
	if err := ValidatePushSource(PushSource{Name: request.Name, Ref: request.SourceRef, ModifiedAt: request.ModifiedAt}); err != nil {
		return err
	}
	if err := validateUUIDv4(request.ScanID); err != nil {
		return err
	}
	if err := validateUUIDv4(request.OperationID); err != nil {
		return err
	}
	if err := ValidatePushDestination(request.ParentPath); err != nil {
		return err
	}
	if normalized, err := NormalizeName(request.FileName); err != nil || normalized != request.FileName {
		return errors.New("invalid publication filename")
	}
	if err := validateNotePolicyFingerprint(request.Hash); err != nil {
		return err
	}
	if request.Size < 0 || request.ExpectedSourceRevision < 0 {
		return errors.New("invalid publication size or expected revision")
	}
	return nil
}

// AcceptPublishedNote atomically fences the verified push bytes, portable
// source observation, positive seen mark and exactly replayable receipt.
func (s *Store) AcceptPublishedNote(ctx context.Context, request NotePublicationUploadRequest, physical ...BlobPhysical) (NotePublicationReceipt, error) {
	var receipt NotePublicationReceipt
	if err := validateNoteUpload(request); err != nil {
		return receipt, err
	}
	digest := publicationRequestDigest(request)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		scan, err := readNoteScanTx(ctx, tx, request.Name, request.ScanID, true)
		if err != nil {
			return err
		}
		var previousDigest, previousScan, encoded string
		err = tx.QueryRowContext(ctx, `SELECT request_sha256,scan_id,receipt_json FROM note_publication_operations WHERE operation_id=$1`, request.OperationID).Scan(&previousDigest, &previousScan, &encoded)
		if err == nil {
			if previousDigest != digest || previousScan != request.ScanID {
				return ErrPublicationOperationMismatch
			}
			return json.Unmarshal([]byte(encoded), &receipt)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if scan.Sealed {
			return ErrPublicationScanSealed
		}
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM note_publication_scans WHERE begin_operation_id=$1)+(SELECT COUNT(*) FROM note_publication_recovery_operations WHERE operation_id=$1)`, request.OperationID).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return ErrPublicationOperationMismatch
		}
		expectedName, err := NormalizeName(path.Base(request.SourceRef))
		if err != nil {
			return err
		}
		expectedParent, err := pushParentSegments(path.Join(scan.Destination, path.Dir(request.SourceRef)))
		if err != nil {
			return err
		}
		actualParent, err := pushParentSegments(request.ParentPath)
		if err != nil {
			return err
		}
		if request.FileName != expectedName || !slices.Equal(actualParent, expectedParent) {
			return ErrInvalidNotePublication
		}
		previous, err := s.publishedNoteSourceTx(ctx, tx, request.Name, request.SourceRef)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if (!exists && request.ExpectedSourceRevision != 0) || (exists && previous.Identity.SourceRevision != request.ExpectedSourceRevision) {
			return ErrPublicationSourceStale
		}
		if exists {
			if previous.Availability == "unavailable" {
				return ErrNoteSourceUnavailable
			}
			if previous.Conflict != "" && previous.Recovery == nil {
				return ErrPublishedSourceDiverged
			}
		} else {
			// First adoption of an existing push/watch identity may use its
			// accepted head, but cannot claim a later independent archive edit.
			state, stateErr := pushStateTx(ctx, tx, request.Name, request.SourceRef)
			if stateErr == nil && (state.Node.BlobHash != state.Hash || state.Node.Size != state.Size) {
				return ErrPublishedSourceDiverged
			}
			if errors.Is(stateErr, ErrExists) {
				return ErrPublishedSourceDiverged
			}
			if stateErr != nil && !errors.Is(stateErr, ErrNotFound) {
				return stateErr
			}
		}
		segments, err := pushParentSegments(request.ParentPath)
		if err != nil {
			return err
		}
		node, outcome, err := s.acceptPushTx(ctx, tx, PushSource{Name: request.Name, Ref: request.SourceRef, ModifiedAt: request.ModifiedAt},
			PushContent{RunID: scan.ScanID, ParentPath: request.ParentPath, Name: request.FileName, Hash: request.Hash, Size: request.Size, MIMEType: "text/markdown"}, segments, exists && previous.Recovery != nil, physical...)
		if errors.Is(err, ErrExists) || errors.Is(err, ErrNotDir) {
			return ErrPublishedSourceDiverged
		}
		if err != nil {
			return err
		}
		if exists && node.ID != previous.Identity.NodeID {
			return ErrPublishedSourceDiverged
		}
		changed := !exists || node.CurrentVersionID != previous.Identity.ContentVersionID || previous.Presence != "present"
		stamp := nowRFC3339()
		revision := int64(1)
		if exists {
			revision = previous.Identity.SourceRevision
			if changed {
				if revision == math.MaxInt64 {
					return errors.New("publication source revision exhausted")
				}
				revision++
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO published_note_sources(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,'present','retained',$8) ON CONFLICT(publication_name,source_ref) DO UPDATE SET
			content_version_id=excluded.content_version_id,blob_hash=excluded.blob_hash,size=excluded.size,source_revision=excluded.source_revision,presence=excluded.presence,availability=excluded.availability,accepted_at=excluded.accepted_at`,
			request.Name, request.SourceRef, node.ID, node.CurrentVersionID, request.Hash, request.Size, revision, stamp)
		if err != nil {
			return err
		}
		if changed {
			if err := bumpNotePublicationTx(ctx, tx, request.Name); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE note_publications SET last_accepted_at=$1 WHERE name=$2`, stamp, request.Name); err != nil {
			return err
		}
		if err = markPublishedNoteSeenTx(ctx, tx, scan.ScanID, request.SourceRef); err != nil {
			return err
		}
		receipt.Source, err = s.publishedNoteSourceTx(ctx, tx, request.Name, request.SourceRef)
		if err != nil {
			return err
		}
		receipt.Scan, receipt.Outcome = scan.NotePublicationScan, outcome
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_publication_operations(operation_id,scan_id,request_sha256,receipt_json) VALUES($1,$2,$3,$4)`, request.OperationID, scan.ScanID, digest, string(data))
		return err
	})
	return receipt, err
}

func bumpNotePublicationTx(ctx context.Context, tx *sql.Tx, name string) error {
	result, err := tx.ExecContext(ctx, `UPDATE note_publications SET namespace_revision=namespace_revision+1 WHERE name=$1 AND namespace_revision<$2`, name, int64(math.MaxInt64))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("publication namespace revision exhausted")
	}
	return err
}

func markPublishedNoteSeenTx(ctx context.Context, tx *sql.Tx, scanID, ref string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO note_publication_seen(scan_id,source_ref) VALUES($1,$2) ON CONFLICT(scan_id,source_ref) DO NOTHING`, scanID, ref)
	return err
}

func samePublishedNoteBinding(a, b PublishedNoteIdentity) bool {
	// Namespace revision changes when another source is accepted. The exact
	// source CAS remains usable during that scan; it fences this binding only.
	a.NamespaceRevision, b.NamespaceRevision = 0, 0
	return a == b
}

func (s *Store) AcknowledgePublishedNotes(ctx context.Context, request NotePublicationSeenRequest) (NotePublicationAck, error) {
	var ack NotePublicationAck
	if err := ValidatePushName(request.Name); err != nil {
		return ack, err
	}
	if err := validateUUIDv4(request.ScanID); err != nil {
		return ack, err
	}
	if len(request.Bindings) > 200 {
		return ack, errors.New("publication seen batch exceeds 200 bindings")
	}
	for _, id := range request.Bindings {
		if err := ValidatePublishedNoteSource(NotePublicationSource{Identity: id, Presence: "present", Availability: "retained"}); err != nil {
			return ack, err
		}
		if id.Publication != request.Name || id.VaultID != s.VaultID() {
			return ack, ErrPublicationSourceStale
		}
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		scan, err := readNoteScanTx(ctx, tx, request.Name, request.ScanID, false)
		if err != nil {
			return err
		}
		ack.Scan = scan.NotePublicationScan
		rows := make([]NotePublicationSource, 0, len(request.Bindings))
		// Validate the entire batch before changing membership or seen marks.
		for _, id := range request.Bindings {
			current, err := s.publishedNoteSourceTx(ctx, tx, request.Name, id.SourceRef)
			if errors.Is(err, ErrNotFound) {
				return ErrPublicationSourceStale
			}
			if err != nil {
				return err
			}
			if !samePublishedNoteBinding(current.Identity, id) {
				return ErrPublicationSourceStale
			}
			if current.Availability == "unavailable" {
				return ErrNoteSourceUnavailable
			}
			if current.Conflict != "" {
				return ErrPublishedSourceDiverged
			}
			rows = append(rows, current)
		}
		changed := false
		for _, source := range rows {
			if source.Presence != "present" {
				result, err := tx.ExecContext(ctx, `UPDATE published_note_sources SET presence='present',source_revision=source_revision+1 WHERE publication_name=$1 AND source_ref=$2 AND presence<>'present' AND source_revision<$3`, request.Name, source.Identity.SourceRef, int64(math.MaxInt64))
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if count == 0 && source.Identity.SourceRevision == math.MaxInt64 {
					return errors.New("publication source revision exhausted")
				}
				changed = changed || count > 0
			}
			if err := markPublishedNoteSeenTx(ctx, tx, scan.ScanID, source.Identity.SourceRef); err != nil {
				return err
			}
		}
		if changed {
			if err := bumpNotePublicationTx(ctx, tx, request.Name); err != nil {
				return err
			}
		}
		ack.Sources = make([]PublishedNoteIdentity, 0, len(rows))
		for _, source := range rows {
			current, err := s.publishedNoteSourceTx(ctx, tx, request.Name, source.Identity.SourceRef)
			if err != nil {
				return err
			}
			ack.Sources = append(ack.Sources, current.Identity)
		}
		return nil
	})
	return ack, err
}

func (s *Store) CompleteNotePublicationScan(ctx context.Context, request NotePublicationCompleteRequest) (NotePublicationStatus, error) {
	var status NotePublicationStatus
	if err := ValidatePushName(request.Name); err != nil {
		return status, err
	}
	if err := validateUUIDv4(request.ScanID); err != nil {
		return status, err
	}
	if err := validateNotePolicyFingerprint(request.PolicyFingerprint); err != nil {
		return status, err
	}
	if !request.InventoryComplete {
		return status, errors.New("publication inventory is incomplete")
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		scan, err := readNoteScanTx(ctx, tx, request.Name, request.ScanID, true)
		if err != nil {
			return err
		}
		if scan.PolicyFingerprint != request.PolicyFingerprint {
			return ErrPublicationOperationMismatch
		}
		if !scan.Sealed {
			var exhausted int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM published_note_sources s WHERE s.publication_name=$1 AND s.presence<>'absent' AND s.source_revision=$2 AND NOT EXISTS(SELECT 1 FROM note_publication_seen x WHERE x.scan_id=$3 AND x.source_ref=s.source_ref)`, request.Name, int64(math.MaxInt64), scan.ScanID).Scan(&exhausted); err != nil {
				return err
			}
			if exhausted != 0 {
				return errors.New("publication source revision exhausted")
			}
			result, err := tx.ExecContext(ctx, `UPDATE published_note_sources SET presence='absent',source_revision=source_revision+1
				WHERE publication_name=$1 AND presence<>'absent' AND NOT EXISTS(SELECT 1 FROM note_publication_seen x WHERE x.scan_id=$2 AND x.source_ref=published_note_sources.source_ref)`, request.Name, scan.ScanID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				if err := bumpNotePublicationTx(ctx, tx, request.Name); err != nil {
					return err
				}
			}
			stamp := nowRFC3339()
			if _, err = tx.ExecContext(ctx, `UPDATE note_publication_scans SET sealed=1,completed_at=$1 WHERE scan_id=$2`, stamp, scan.ScanID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM note_publication_seen WHERE scan_id=$1`, scan.ScanID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE note_publications SET last_complete_at=$1 WHERE name=$2`, stamp, request.Name); err != nil {
				return err
			}
		}
		status, err = notePublicationStatusTx(ctx, tx, request.Name)
		return err
	})
	return status, err
}
