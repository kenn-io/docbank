package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"go.kenn.io/docbank/internal/home"
)

// PhotoCheckoutObservation records machine-local working-copy settlement.
type PhotoCheckoutObservation struct {
	FileID   string `json:"FileID"`
	Identity string `json:"Identity"`
	Hash     string `json:"Hash"`
	Size     int64  `json:"Size"`
	Mtime    string `json:"Mtime"`
	Since    string `json:"Since"`
	State    string `json:"State"`
}

func (s *Store) ObservePhotoCheckoutEntries(ctx context.Context, id, destinationIdentity string, observations []PhotoCheckoutObservation) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT destination_identity=$1 AND retired_at IS NULL FROM photo_checkouts WHERE checkout_id=$2`, destinationIdentity, id).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrInvalidPhotoCheckout
		}
		return s.observePhotoCheckoutEntriesTx(ctx, tx, id, observations, false)
	})
}

func (s *Store) observePhotoCheckoutEntriesTx(ctx context.Context, tx *sql.Tx, id string, observations []PhotoCheckoutObservation, bind bool) error {
	const batchSize = 256
	query := s.photoCheckoutObservationsSQL(bind)
	for start := 0; start < len(observations); start += batchSize {
		batch := observations[start:min(start+batchSize, len(observations))]
		encoded, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if len(encoded) > 128<<10 {
			return fmt.Errorf("%w: checkout observation batch exceeds 128 KiB", ErrInvalidPhotoCheckout)
		}
		if _, err := tx.ExecContext(ctx, query, string(encoded), id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) photoCheckoutObservationsSQL(bind bool) string {
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	values := d.arrayValues("$1")
	join := "JOIN"
	if !d.postgres {
		values += " AS items"
		// Keep the bounded observation batch outermost without SQLite statistics.
		join = "CROSS JOIN"
	}
	binding := ""
	if bind {
		binding = `,published=CASE WHEN o.identity<>'' THEN 1 ELSE 0 END,base_held=CASE WHEN e.base_held<>0 OR (o.identity<>'' AND EXISTS(SELECT 1 FROM content_versions v WHERE v.version_id=e.version_id)) THEN 1 ELSE 0 END`
	}
	return `WITH changes AS MATERIALIZED (
 SELECT matched.checkout_id,matched.file_id,` + d.text("items.value", "Identity") + ` AS identity,
 ` + d.text("items.value", "Hash") + ` AS hash,` + d.number("items.value", "Size") + ` AS size,
 ` + d.text("items.value", "Mtime") + ` AS mtime,` + d.text("items.value", "Since") + ` AS since,` + d.text("items.value", "State") + ` AS state
 FROM ` + values + ` ` + join + ` photo_checkout_entries matched
 ON matched.checkout_id=$2 AND matched.file_id=` + d.text("items.value", "FileID") + `)
 UPDATE photo_checkout_entries AS e
 SET observed_identity=o.identity,observed_hash=o.hash,
 observed_size=o.size,observed_mtime=o.mtime,observed_since=o.since,
 working_state=CASE WHEN o.state<>'missing' AND o.hash<>''
 AND (o.hash<>e.blob_hash OR o.size<>e.size)
 AND NOT EXISTS(SELECT 1 FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE f.file_id=e.file_id AND ` + photoCheckoutEntryAuthority + `)
 THEN 'conflict' WHEN o.state='edited' AND e.blob_hash=o.hash AND e.size=o.size
 THEN 'clean' ELSE o.state END` + binding + `
 FROM changes o WHERE e.checkout_id=o.checkout_id AND e.file_id=o.file_id`
}

// BindPhotoCheckout records a verified machine-local root; backups omit it.
// The caller checks the previous folder before entering this write transaction.
func (s *Store) BindPhotoCheckout(ctx context.Context, id, destination, identity, operationID string, observations []PhotoCheckoutObservation) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		c, err := scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=$1`, id))
		if err != nil {
			return err
		}
		if err := ValidatePhotoCheckoutRebind(c, operationID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE photo_checkouts SET destination=$1,destination_identity=$2,state=CASE WHEN state='unbound' THEN 'completed' ELSE state END,destination_reserved=1 WHERE checkout_id=$3`, destination, identity, id)
		if err != nil {
			return err
		}
		if err := s.observePhotoCheckoutEntriesTx(ctx, tx, id, observations, true); err != nil {
			return err
		}
		return validatePhotoCheckoutReferences(ctx, tx)
	})
}

// ValidatePhotoCheckoutScanOrCommit requires a bound, idle checkout before local checks.
func ValidatePhotoCheckoutScanOrCommit(c PhotoCheckout, kind string) error {
	operation := "scan"
	if kind == StorageOperationKindPhotoCheckoutCommit {
		operation = "commit"
	}
	if c.RetiredAt != nil || c.RetirementRequested || photoCheckoutOperationActive(c) || c.State == "unbound" || c.Destination == "" || !c.DestinationReserved {
		return fmt.Errorf("%w: checkout must be bound and idle before %s", ErrInvalidPhotoCheckout, operation)
	}
	return nil
}

// ValidatePhotoCheckoutRebind permits an idle checkout or its admitted rebind operation.
func ValidatePhotoCheckoutRebind(c PhotoCheckout, operationID string) error {
	if c.RetiredAt == nil && (!c.RetirementRequested && !photoCheckoutOperationActive(c) || operationID != "" && c.LastOperationID == operationID && c.LastOperationKind == StorageOperationKindPhotoCheckoutRebind) {
		return nil
	}
	return fmt.Errorf("%w: checkout must be idle and available before rebind", ErrInvalidPhotoCheckout)
}

// ValidatePhotoCheckoutRebindDestination preserves a previous folder's reservation
// while it still contains copies, unless the verified destination is that folder.
func ValidatePhotoCheckoutRebindDestination(c PhotoCheckout, identity string) error {
	if c.Destination == "" {
		return nil
	}
	root, err := openPreviousPhotoCheckoutRoot(c.Destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: opening previous folder: %w", ErrInvalidPhotoCheckout, err)
	}
	defer func() { _ = root.Close() }()
	folder, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("%w: opening previous folder: %w", ErrInvalidPhotoCheckout, err)
	}
	defer func() { _ = folder.Close() }()
	previousIdentity, err := home.FileIdentity(folder)
	if err != nil {
		return fmt.Errorf("%w: identifying previous folder: %w", ErrInvalidPhotoCheckout, err)
	}
	if previousIdentity == identity {
		return nil
	}
	entries, err := folder.ReadDir(1)
	if len(entries) > 0 {
		return fmt.Errorf("%w: clear or release the previous folder first", ErrInvalidPhotoCheckout)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: reading previous folder: %w", ErrInvalidPhotoCheckout, err)
	}
	return nil
}

func openPreviousPhotoCheckoutRoot(destination string) (*os.Root, error) {
	root, err := os.OpenRoot(destination)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		if info, statErr := os.Stat(destination); statErr == nil && !info.IsDir() {
			return nil, os.ErrNotExist
		}
	}
	return root, err
}

func photoCheckoutOperationActive(c PhotoCheckout) bool {
	return c.LastOperationState == string(StorageOperationQueued) || c.LastOperationState == string(StorageOperationRunning)
}

type checkoutCommitState struct {
	CheckoutID  string `json:"checkout_id"`
	OperationID string `json:"operation_id"`
	FileID      string `json:"file_id"`
	VersionID   string `json:"version_id"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
}

// CommitPhotoCheckoutEntry binds a verified blob, the advanced checkout base,
// its receipt and saved-version counters in the same transaction.
func (s *Store) CommitPhotoCheckoutEntry(ctx context.Context, c PhotoCheckout, e PhotoCheckoutEntry, hash string, size int64, mediaType string, physical BlobPhysical) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		exists, err := s.checkoutCommitEntryRecorded(ctx, tx, c.LastOperationID, e.FileID)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		nodeID, err := checkoutCommitAuthority(ctx, tx, c, e)
		if err != nil {
			return err
		}
		node, err := nodeByIDTx(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		before := checkoutCommitState{c.ID, c.LastOperationID, e.FileID, e.VersionID, e.Hash, e.Size}
		var originalMtime string
		// Match photo_timeline_refresh in schema.sql: a revert inherits the
		// earliest unsuperseded mtime bound to the same node and blob.
		if err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain(observed_at,identity) AS (
  SELECT b.observed_at,b.provenance_identity FROM content_versions cv
  JOIN provenance_version_bindings b ON b.content_version_id=cv.version_id
  WHERE cv.node_id=$1 AND cv.blob_hash=$2
  UNION ALL SELECT chain.observed_at,p.identity FROM chain JOIN provenance p ON p.supersedes=chain.identity)
  SELECT COALESCE((SELECT p.original_mtime FROM chain JOIN provenance p ON p.identity=chain.identity
  WHERE p.original_mtime IS NOT NULL AND substr(p.original_mtime,1,4)<>'0000'
  AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
  ORDER BY chain.observed_at,p.identity LIMIT 1),'')`, nodeID, e.Hash).Scan(&originalMtime); err != nil {
			return err
		}
		replaced, version, err := s.replaceContentTx(ctx, tx, node, node.Revision, hash, size, mediaType, physical)
		if err != nil {
			return err
		}
		var startedAt string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM storage_operations WHERE operation_id=$1`, c.LastOperationID).Scan(&startedAt); err != nil {
			return err
		}
		run := IngestRun{record: metadataIngest{Type: metadataIngestType, ID: c.LastOperationID, StartedAt: startedAt, SourceKind: "photo-checkout", SourceDesc: c.ID}}
		if _, err := s.observePhotoImportSourceTx(ctx, tx, run, replaced, PhotoImportMember{OriginalPath: e.Path, OriginalMtime: originalMtime}, version.ID); err != nil {
			return err
		}
		after := checkoutCommitState{c.ID, c.LastOperationID, e.FileID, version.ID, hash, size}
		if err := s.writePhotoReceiptTx(ctx, tx, photoReceipt{Operation: "checkout_commit", AssetID: e.AssetID, BeforeRevision: node.Revision, AfterRevision: version.NodeRevision, Before: before, After: after}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET version_id=$1,blob_hash=$2,size=$3,base_held=1,working_state='clean' WHERE checkout_id=$4 AND file_id=$5`, version.ID, hash, size, c.ID, e.FileID); err != nil {
			return err
		}
		return advanceCheckoutCommitTx(ctx, tx, c.LastOperationID, size)
	})
}

func advanceCheckoutCommitTx(ctx context.Context, tx *sql.Tx, operationID string, size int64) error {
	// Copied counters report saved versions; receipts identify entries on retry.
	result, err := tx.ExecContext(ctx, `UPDATE storage_operations SET copied_objects=copied_objects+1,copied_bytes=copied_bytes+$1,updated_at=$2 WHERE operation_id=$3 AND state='running'`, size, nowRFC3339(), operationID)
	return requireOneStorageOperationRow(result, err, operationID, "advancing checkout commit")
}

func retireRequestedCheckoutTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET retired_at=$1,retention_released=1,destination_reserved=0 WHERE checkout_id=$2 AND retirement_requested=1 AND retired_at IS NULL`, nowRFC3339(), id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET base_held=0 WHERE checkout_id=$1 AND EXISTS(SELECT 1 FROM photo_checkouts WHERE checkout_id=$2 AND retired_at IS NOT NULL)`, id, id)
	return err
}

func validateCheckoutCommitReceipt(v metadataPhotoReceipt) error {
	if v.AssetID == nil || validateUUIDv4(*v.AssetID) != nil || v.SettingsKey != nil || v.SetID != nil || v.BeforeRevision < 1 || v.AfterRevision != v.BeforeRevision+1 {
		return ErrInvalidPhotoCheckout
	}
	var before, after checkoutCommitState
	if err := json.Unmarshal([]byte(v.BeforeJSON), &before, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(v.AfterJSON), &after, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for _, state := range []checkoutCommitState{before, after} {
		if validateUUIDv4(state.CheckoutID) != nil || validateUUIDv4(state.OperationID) != nil || validateUUIDv4(state.FileID) != nil || validateUUIDv4(state.VersionID) != nil || validateCatalogSHA256(state.Hash, "checkout commit hash") != nil || state.Size < 0 {
			return ErrInvalidPhotoCheckout
		}
	}
	if before.CheckoutID != after.CheckoutID || before.OperationID != after.OperationID || before.FileID != after.FileID || before.VersionID == after.VersionID {
		return ErrInvalidPhotoCheckout
	}
	return validateMetadataTime("checkout commit receipt created_at", v.CreatedAt)
}

// photoCheckoutEntryAuthority compares the frozen selection with current authority.
const photoCheckoutEntryAuthority = `f.asset_id=e.asset_id AND f.sidecar_of_file_id IS NOT DISTINCT FROM e.sidecar_of_file_id AND n.current_version_id=e.version_id AND n.trashed_at IS NULL`

// CheckPhotoCheckoutCommitEntry reports an already committed entry and refuses stale
// working edits before blob publication.
func (s *Store) CheckPhotoCheckoutCommitEntry(ctx context.Context, c PhotoCheckout, e PhotoCheckoutEntry) (bool, error) {
	var committed bool
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		committed, err = s.checkoutCommitEntryRecorded(ctx, tx, c.LastOperationID, e.FileID)
		if err != nil || committed {
			return err
		}
		_, err = checkoutCommitAuthority(ctx, tx, c, e)
		return err
	})
	return committed, err
}

func (s *Store) checkoutCommitEntryRecorded(ctx context.Context, q metadataQuerier, operationID, fileID string) (bool, error) {
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_change_receipts WHERE operation='checkout_commit' AND `+d.text(d.receiptColumn(""), "operation_id")+`=$1 AND `+d.text(d.receiptColumn(""), "file_id")+`=$2)`, operationID, fileID).Scan(&exists)
	return exists, err
}

func checkoutCommitAuthority(ctx context.Context, q metadataQuerier, c PhotoCheckout, e PhotoCheckoutEntry) (int64, error) {
	var nodeID int64
	var valid bool
	if err := q.QueryRowContext(ctx, `SELECT f.node_id,COALESCE(e.version_id=$1 AND e.blob_hash=$2 AND e.size=$3 AND `+photoCheckoutEntryAuthority+` AND c.last_operation_id=$4 AND EXISTS(SELECT 1 FROM storage_operations o WHERE o.operation_id=c.last_operation_id AND o.kind='photo_checkout_commit') AND c.destination_identity=$5 AND c.retired_at IS NULL,FALSE)
  FROM photo_checkout_entries e JOIN photo_checkouts c ON c.checkout_id=e.checkout_id JOIN photo_files f ON f.file_id=e.file_id JOIN nodes n ON n.id=f.node_id WHERE e.checkout_id=$6 AND e.file_id=$7`, e.VersionID, e.Hash, e.Size, c.LastOperationID, c.DestinationIdentity, c.ID, e.FileID).Scan(&nodeID, &valid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: checkout file binding changed", ErrStaleRevision)
		}
		return 0, err
	}
	if !valid {
		return 0, fmt.Errorf("%w: checkout file binding changed", ErrStaleRevision)
	}
	return nodeID, nil
}
