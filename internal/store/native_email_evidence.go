package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"go.kenn.io/docbank/document"
)

// NativeEmailEvidenceSelection names one retained EML version and decoded
// generation. Callers must authorize the vault read and pass the active owner
// for mailbox occurrence disclosure.
type NativeEmailEvidenceSelection struct {
	NodeID       int64
	VersionID    string
	BlobSHA256   string
	GenerationID string
}

// NativeMailboxOccurrence identifies a source observation, not another message.
// A direct archive transfer has only ArchiveID and ReceiptID. A mailbox job
// observation also has JobID and Ordinal.
type NativeMailboxOccurrence struct {
	ArchiveID string
	SourceRef string
	ReceiptID string
	JobID     string
	Ordinal   int64
	Location  *MailboxLocation
}

// NativeEmailEvidence is one read snapshot. State is available only for a
// verified complete inventory rooted at path 1; other states are missing,
// withdrawn, or unavailable. BodySearch independently describes whether text
// currently serves search. BodyBuild is populated only for that serving build;
// a retained superseded generation remains available with BodySearch unavailable.
// A frozen report must retain the exact build identity at observation time and
// replay that retained authority by ID, not infer it from a later serving head.
// OccurrenceState is available or unavailable; an unavailable occurrence set
// must not be counted as zero.
type NativeEmailEvidence struct {
	State            string
	Reason           string
	Source           NodeView
	Metadata         EmailMetadataView
	RootMessage      document.EmailMessageV1
	BodyBuild        *RenditionBuildRecord
	OccurrenceState  string
	OccurrenceReason string
	Occurrences      []NativeMailboxOccurrence
}

const (
	maxNativeEmailOccurrences      = 1000
	nativeEmailAvailable           = "available"
	nativeEmailUnavailable         = "unavailable"
	nativeEmailSnapshotUnavailable = "snapshot_unavailable"
)

// NativeEmailEvidence reads exact native email and retained source evidence
// without starting processing, rendering, or an import. All fields come from
// one SQLite snapshot, so a concurrent edit or trash cannot mix authorities.
func (s *Store) NativeEmailEvidence(ctx context.Context, owner string, selected NativeEmailEvidenceSelection) (NativeEmailEvidence, error) {
	if owner == "" || selected.NodeID < 1 || selected.VersionID == "" || selected.BlobSHA256 == "" || selected.GenerationID == "" {
		return NativeEmailEvidence{State: "missing", Reason: "invalid_selection", OccurrenceState: nativeEmailUnavailable, OccurrenceReason: nativeEmailSnapshotUnavailable}, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	defer func() { _ = tx.Rollback() }()
	node, err := nodeByIDQuery(ctx, tx, selected.NodeID)
	if errors.Is(err, ErrNotFound) {
		return NativeEmailEvidence{State: "missing", Reason: "node_missing", OccurrenceState: nativeEmailUnavailable, OccurrenceReason: nativeEmailSnapshotUnavailable}, nil
	}
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	version, err := emailVersion(ctx, tx, selected.VersionID)
	if errors.Is(err, ErrNotFound) || (err == nil && version.NodeID != node.ID) {
		return NativeEmailEvidence{State: "missing", Reason: "version_missing", OccurrenceState: nativeEmailUnavailable, OccurrenceReason: nativeEmailSnapshotUnavailable}, nil
	}
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	source, err := nodeViewForNode(ctx, tx, node)
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	result := NativeEmailEvidence{Source: source, OccurrenceState: nativeEmailUnavailable, OccurrenceReason: nativeEmailSnapshotUnavailable}
	if node.TrashedAt != nil {
		result.State, result.Reason = "withdrawn", "node_trashed"
		return result, nil
	}
	if version.BlobHash != selected.BlobSHA256 {
		result.State, result.Reason = nativeEmailUnavailable, "blob_hash_mismatch"
		return result, nil
	}
	metadata, err := emailMetadataView(ctx, tx, version, selected.GenerationID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrEmailPending) || errors.Is(err, ErrEmailNotSupported) || errors.Is(err, ErrEmailDerivativeSuppressed) {
		result.State, result.Reason = nativeEmailUnavailable, "generation_unavailable"
		return result, nil
	}
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	result.Metadata = metadata
	if metadata.Evidence.Inventory == nil {
		result.State, result.Reason = nativeEmailUnavailable, "inventory_unavailable"
		return result, nil
	}
	if metadata.Evidence.Inventory.State != document.EmailInventoryComplete || metadata.Evidence.Inventory.RootPath == nil || *metadata.Evidence.Inventory.RootPath != "1" {
		result.State, result.Reason = nativeEmailUnavailable, "incomplete_inventory"
		return result, nil
	}
	for _, message := range metadata.Evidence.Inventory.Messages {
		if message.Path == "1" {
			result.RootMessage = message
			break
		}
	}
	if result.RootMessage.Path != "1" {
		result.State, result.Reason = nativeEmailUnavailable, "root_message_unavailable"
		return result, nil
	}
	build, serving, err := nativeEmailBodyBuild(ctx, tx, s.vaultID, metadata)
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	if serving {
		result.BodyBuild = &build
	}
	occurrences, occurrenceReason, err := nativeMailboxOccurrences(ctx, tx, owner, version, node.Revision)
	if err != nil {
		return NativeEmailEvidence{}, err
	}
	if occurrenceReason != "" {
		result.OccurrenceReason = occurrenceReason
	} else {
		result.OccurrenceState = nativeEmailAvailable
		result.OccurrenceReason = ""
		result.Occurrences = occurrences
	}
	result.State = nativeEmailAvailable
	if err := tx.Commit(); err != nil {
		return NativeEmailEvidence{}, err
	}
	return result, nil
}

func nativeEmailBodyBuild(ctx context.Context, q metadataQuerier, vaultID string, metadata EmailMetadataView) (RenditionBuildRecord, bool, error) {
	if metadata.BodySearch.State != nativeEmailAvailable {
		return RenditionBuildRecord{}, false, nil
	}
	if metadata.BodySearch.RenditionBuildID == nil || *metadata.BodySearch.RenditionBuildID == "" || metadata.BodySearch.RenditionAttachmentID == nil || *metadata.BodySearch.RenditionAttachmentID == "" {
		return RenditionBuildRecord{}, false, fmt.Errorf("%w: available body has no build or attachment identity", ErrEmailCorrupt)
	}
	build, err := loadRenditionBuild(ctx, q, *metadata.BodySearch.RenditionBuildID)
	if err != nil {
		return RenditionBuildRecord{}, false, err
	}
	if build.VaultID != vaultID || build.ID != *metadata.BodySearch.RenditionBuildID {
		return RenditionBuildRecord{}, false, fmt.Errorf("%w: body build identity", ErrEmailCorrupt)
	}
	return build, true, nil
}

func nativeMailboxOccurrences(ctx context.Context, q metadataQuerier, owner string, version ContentVersion, nodeRevision int64) ([]NativeMailboxOccurrence, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id FROM mailbox_transfer_receipts r JOIN mailbox_archives a ON a.id=r.archive_id WHERE r.target_version_id=? AND a.owner=? ORDER BY r.id LIMIT ?`, version.ID, owner, maxNativeEmailOccurrences+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, "", err
	}
	if len(ids) > maxNativeEmailOccurrences {
		return nil, "occurrence_limit", nil
	}
	out := make([]NativeMailboxOccurrence, 0, len(ids))
	for _, id := range ids {
		receipt, loadErr := loadMailboxTransferReceipt(ctx, q, id)
		if loadErr != nil {
			return nil, "", loadErr
		}
		if receipt.Target != documentIdentity(version) || receipt.TargetRevision > nodeRevision || (version.NodeRevision > 0 && receipt.TargetRevision < version.NodeRevision) {
			return nil, "", ErrMailboxInvalid
		}
		switch receipt.OriginKind {
		case "job":
			if receipt.OccurrenceOverflow {
				return nil, "occurrence_limit", nil
			}
			if len(receipt.OccurrenceKeys) > maxNativeEmailOccurrences-len(out) {
				return nil, "occurrence_limit", nil
			}
			if err := nativeReceiptKeysComplete(ctx, q, receipt); err != nil {
				return nil, "", err
			}
			for _, key := range receipt.OccurrenceKeys {
				occurrence, readErr := nativeReceiptOccurrenceByKey(ctx, q, owner, receipt, key)
				if readErr != nil {
					return nil, "", readErr
				}
				out = append(out, occurrence)
			}
		case "":
			possibleJob, originErr := nativeLegacyReceiptMayHaveJobOrigin(ctx, q, receipt)
			if originErr != nil {
				return nil, "", originErr
			}
			if !possibleJob {
				var linked bool
				if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_occurrences WHERE receipt_id=?)`, receipt.ID).Scan(&linked); err != nil {
					return nil, "", err
				}
				possibleJob = linked
			}
			if possibleJob {
				return nil, "legacy_occurrence_unproven", nil
			}
			fallthrough
		case "direct":
			jobBound, bindErr := nativeReceiptHasExactJobBinding(ctx, q, receipt)
			if bindErr != nil {
				return nil, "", bindErr
			}
			if jobBound {
				return nil, "", ErrMailboxInvalid
			}
			jobOccurrences, bounded, readErr := nativeReceiptJobOccurrences(ctx, q, owner, receipt, maxNativeEmailOccurrences-len(out))
			if readErr != nil {
				return nil, "", readErr
			}
			if !bounded {
				return nil, "occurrence_limit", nil
			}
			if len(jobOccurrences) != 0 {
				return nil, "", ErrMailboxInvalid
			}
			out = append(out, NativeMailboxOccurrence{ArchiveID: receipt.Request.ArchiveID, SourceRef: receipt.Request.Reference, ReceiptID: id, Location: receipt.Location})
			if len(out) > maxNativeEmailOccurrences {
				return nil, "occurrence_limit", nil
			}
		default:
			return nil, "", ErrMailboxInvalid
		}
	}
	return out, "", nil
}

// nativeReceiptKeysComplete checks the normalized links as well as retained
// keys. The result is bounded, though receipt_id has no index in this layout.
func nativeReceiptKeysComplete(ctx context.Context, q metadataQuerier, receipt MailboxTransferReceipt) error {
	expected := make(map[MailboxReceiptOccurrenceKey]bool, len(receipt.OccurrenceKeys))
	for _, key := range receipt.OccurrenceKeys {
		expected[key] = true
	}
	rows, err := q.QueryContext(ctx, `SELECT job_id,ordinal FROM mailbox_occurrences WHERE receipt_id=? LIMIT ?`, receipt.ID, len(expected)+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[MailboxReceiptOccurrenceKey]bool, len(expected))
	for rows.Next() {
		var key MailboxReceiptOccurrenceKey
		if err := rows.Scan(&key.JobID, &key.Ordinal); err != nil {
			return err
		}
		if !expected[key] || seen[key] {
			return ErrMailboxInvalid
		}
		seen[key] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return ErrMailboxInvalid
	}
	return nil
}

func nativeLegacyReceiptMayHaveJobOrigin(ctx context.Context, q metadataQuerier, receipt MailboxTransferReceipt) (bool, error) {
	collectionID, ok := nativeMailboxCollectionID(receipt.Request.ArchiveID)
	if !ok {
		return false, nil
	}
	// An unmarked legacy replacement has no stored writer identity. Retained
	// node provenance makes job origin possible, so occurrence coverage is
	// unavailable instead of guessed to be complete. The receipt is owner-scoped.
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ingests i JOIN provenance p ON p.ingest_id=i.id AND p.node_id=? WHERE i.id=? AND i.source_kind='mailbox')`, receipt.Target.NodeID, collectionID).Scan(&found)
	return found, err
}

func nativeReceiptHasExactJobBinding(ctx context.Context, q metadataQuerier, receipt MailboxTransferReceipt) (bool, error) {
	collectionID, ok := nativeMailboxCollectionID(receipt.Request.ArchiveID)
	if !ok {
		return false, nil
	}
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ingests i JOIN provenance p ON p.ingest_id=i.id AND p.node_id=? JOIN provenance_version_bindings b ON b.provenance_identity=p.identity AND b.content_version_id=? WHERE i.id=? AND i.source_kind='mailbox')`, receipt.Target.NodeID, receipt.Target.VersionID, collectionID).Scan(&found)
	return found, err
}

func nativeMailboxCollectionID(archiveID string) (string, bool) {
	collectionID, ok := strings.CutPrefix(archiveID, "mailbox:")
	if !ok {
		return "", false
	}
	return collectionID, validateUUIDv4(collectionID) == nil
}

func nativeReceiptJobOccurrences(ctx context.Context, q metadataQuerier, owner string, receipt MailboxTransferReceipt, remaining int) ([]NativeMailboxOccurrence, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT job_id,ordinal,receipt_id,occurrence_json FROM mailbox_occurrences WHERE receipt_id=? ORDER BY job_id,ordinal LIMIT ?`, receipt.ID, remaining+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := []NativeMailboxOccurrence{}
	for rows.Next() {
		var jobID string
		var ordinal int64
		var linkedReceiptID string
		var raw []byte
		if err = rows.Scan(&jobID, &ordinal, &linkedReceiptID, &raw); err != nil {
			return nil, false, err
		}
		occurrence, validationErr := nativeValidatedReceiptOccurrence(ctx, q, owner, receipt, jobID, ordinal, linkedReceiptID, raw)
		if validationErr != nil {
			return nil, false, validationErr
		}
		out = append(out, occurrence)
		if len(out) > remaining {
			return nil, false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func nativeReceiptOccurrenceByKey(ctx context.Context, q metadataQuerier, owner string, receipt MailboxTransferReceipt, key MailboxReceiptOccurrenceKey) (NativeMailboxOccurrence, error) {
	var linkedReceiptID sql.NullString
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT receipt_id,occurrence_json FROM mailbox_occurrences WHERE job_id=? AND ordinal=?`, key.JobID, key.Ordinal).Scan(&linkedReceiptID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return NativeMailboxOccurrence{}, ErrMailboxInvalid
	}
	if err != nil {
		return NativeMailboxOccurrence{}, err
	}
	return nativeValidatedReceiptOccurrence(ctx, q, owner, receipt, key.JobID, key.Ordinal, linkedReceiptID.String, raw)
}

func nativeValidatedReceiptOccurrence(ctx context.Context, q metadataQuerier, owner string, receipt MailboxTransferReceipt, jobID string, ordinal int64, linkedReceiptID string, raw []byte) (NativeMailboxOccurrence, error) {
	if linkedReceiptID != receipt.ID {
		return NativeMailboxOccurrence{}, ErrMailboxInvalid
	}
	if len(raw) > 64<<10 {
		return NativeMailboxOccurrence{}, ErrMailboxLimit
	}
	var occurrence MailboxOccurrence
	if err := json.Unmarshal(raw, &occurrence, json.RejectUnknownMembers(true)); err != nil {
		return NativeMailboxOccurrence{}, err
	}
	job, err := loadMailboxJob(ctx, q, owner, jobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrMailboxInvalid) {
			return NativeMailboxOccurrence{}, fmt.Errorf("%w: mailbox job owner or identity", ErrMailboxInvalid)
		}
		return NativeMailboxOccurrence{}, fmt.Errorf("loading mailbox job %s: %w", jobID, err)
	}
	settings, err := job.Settings.Canonical()
	if err != nil {
		return NativeMailboxOccurrence{}, err
	}
	if occurrence.JobID != jobID || occurrence.Ordinal != ordinal || occurrence.ReceiptID != receipt.ID || occurrence.Outcome != "imported" || occurrence.Target == nil || *occurrence.Target != receipt.Target || receipt.Location == nil || !reflect.DeepEqual(*receipt.Location, occurrence.Location) || receipt.Request.ArchiveID != "mailbox:"+job.CollectionID || receipt.Request.Reference != fmt.Sprintf("%d:%d", occurrence.Location.EntryIndex, occurrence.Location.Sequence) || receipt.Request.Settings != settings || receipt.Request.DestinationID != job.Settings.DestinationID || occurrence.Location.ContainerID != job.ContainerID {
		return NativeMailboxOccurrence{}, ErrMailboxInvalid
	}
	location := occurrence.Location
	return NativeMailboxOccurrence{ArchiveID: receipt.Request.ArchiveID, SourceRef: receipt.Request.Reference, ReceiptID: receipt.ID, JobID: jobID, Ordinal: ordinal, Location: &location}, nil
}
