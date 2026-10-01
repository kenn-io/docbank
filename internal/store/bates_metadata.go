package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfstamp"
)

const (
	metadataBatesNamespace       = "bates_namespace"
	metadataBatesNamespaceCursor = "bates_namespace_cursor"
	metadataBatesAllocation      = "bates_allocation"
	metadataBatesPageLabel       = "bates_page_label"
	metadataBatesArtifact        = "bates_artifact"
	metadataBatesArtifactPage    = "bates_artifact_page"
)

// Explicit JSON tags keep JSONL stable across physical schema upgrades.
type metadataBatesNamespaceRecord struct {
	Type        string `json:"type"`
	NamespaceID string `json:"namespace_id" db:"namespace_id"`
	Prefix      string `json:"prefix" db:"prefix"`
	Suffix      string `json:"suffix" db:"suffix"`
	Padding     int    `json:"padding" db:"padding"`
	CreatedAt   string `json:"created_at" db:"created_at"`
}
type metadataBatesCursorRecord struct {
	Type         string `json:"type"`
	NamespaceID  string `json:"namespace_id" db:"namespace_id"`
	NextSequence int64  `json:"next_sequence" db:"next_sequence"`
}
type metadataBatesAllocationRecord struct {
	Type          string  `json:"type"`
	AllocationID  string  `json:"allocation_id" db:"allocation_id"`
	OperationID   string  `json:"operation_id" db:"operation_id"`
	NamespaceID   string  `json:"namespace_id" db:"namespace_id"`
	SnapshotID    string  `json:"snapshot_id" db:"snapshot_id"`
	RequestSHA256 string  `json:"request_sha256" db:"request_sha256"`
	RecipeSHA256  string  `json:"recipe_sha256" db:"recipe_sha256"`
	StartSequence int64   `json:"start_sequence" db:"start_sequence"`
	EndSequence   int64   `json:"end_sequence" db:"end_sequence"`
	State         string  `json:"state" db:"state"`
	CreatedAt     string  `json:"created_at" db:"created_at"`
	CommittedAt   *string `json:"committed_at" db:"committed_at"`
}
type metadataBatesPageLabelRecord struct {
	Type         string `json:"type"`
	AllocationID string `json:"allocation_id" db:"allocation_id"`
	Ordinal      int    `json:"ordinal" db:"ordinal"`
	NamespaceID  string `json:"namespace_id" db:"namespace_id"`
	Sequence     int64  `json:"sequence" db:"sequence"`
	OccurrenceID string `json:"occurrence_id" db:"occurrence_id"`
	SourcePage   int    `json:"source_page" db:"source_page"`
	OutputPage   int    `json:"output_page" db:"output_page"`
	Label        string `json:"label" db:"label"`
}
type metadataBatesArtifactRecord struct {
	Type           string         `json:"type"`
	ArtifactID     string         `json:"artifact_id" db:"artifact_id"`
	AllocationID   string         `json:"allocation_id" db:"allocation_id"`
	BlobSHA256     string         `json:"blob_sha256" db:"blob_hash"`
	Size           int64          `json:"size" db:"size"`
	MediaType      string         `json:"media_type" db:"media_type"`
	PageCount      int            `json:"page_count" db:"page_count"`
	RecipeJSON     jsontext.Value `json:"recipe_json" db:"recipe_json"`
	ManifestSHA256 string         `json:"manifest_sha256" db:"manifest_sha256"`
	State          string         `json:"state" db:"state"`
	CreatedAt      string         `json:"created_at" db:"created_at"`
}
type metadataBatesArtifactPageRecord struct {
	Type             string `json:"type"`
	ArtifactID       string `json:"artifact_id" db:"artifact_id"`
	Ordinal          int    `json:"ordinal" db:"ordinal"`
	OccurrenceID     string `json:"occurrence_id" db:"occurrence_id"`
	SourceBlobSHA256 string `json:"source_blob_sha256" db:"source_blob_sha256"`
	SourcePage       int    `json:"source_page" db:"source_page"`
	OutputPage       int    `json:"output_page" db:"output_page"`
	Label            string `json:"label" db:"label"`
}

func exportBatesMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	ids, err := pageMetadataKeys(ctx, q, `SELECT namespace_id FROM bates_namespaces ORDER BY namespace_id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var r metadataBatesNamespaceRecord
		r.Type = metadataBatesNamespace
		if err := q.QueryRowContext(ctx, `SELECT namespace_id,prefix,suffix,padding,created_at FROM bates_namespaces WHERE namespace_id=?`, id).
			Scan(&r.NamespaceID, &r.Prefix, &r.Suffix, &r.Padding, &r.CreatedAt); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
		var cursor metadataBatesCursorRecord
		cursor.Type = metadataBatesNamespaceCursor
		if err := q.QueryRowContext(ctx, `SELECT namespace_id,next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`, id).
			Scan(&cursor.NamespaceID, &cursor.NextSequence); err != nil {
			return err
		}
		if err := write(cursor); err != nil {
			return err
		}
	}
	allocationIDs, err := pageMetadataKeys(ctx, q, `SELECT allocation_id FROM bates_allocations ORDER BY allocation_id`)
	if err != nil {
		return err
	}
	for _, id := range allocationIDs {
		var r metadataBatesAllocationRecord
		var committed sql.NullString
		r.Type = metadataBatesAllocation
		if err := q.QueryRowContext(ctx, `SELECT allocation_id,operation_id,namespace_id,snapshot_id,request_sha256,recipe_sha256,
			start_sequence,end_sequence,state,created_at,committed_at FROM bates_allocations WHERE allocation_id=?`, id).
			Scan(&r.AllocationID, &r.OperationID, &r.NamespaceID, &r.SnapshotID, &r.RequestSHA256, &r.RecipeSHA256,
				&r.StartSequence, &r.EndSequence, &r.State, &r.CreatedAt, &committed); err != nil {
			return err
		}
		if committed.Valid {
			r.CommittedAt = &committed.String
		}
		if err := write(r); err != nil {
			return err
		}
		if err := exportBatesLabels(ctx, q, write, id); err != nil {
			return err
		}
	}
	return nil
}

func exportBatesArtifactMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	artifactIDs, err := pageMetadataKeys(ctx, q, `SELECT artifact_id FROM bates_artifacts ORDER BY artifact_id`)
	if err != nil {
		return err
	}
	for _, id := range artifactIDs {
		artifact, err := loadBatesArtifact(ctx, q, id)
		if err != nil {
			return err
		}
		if err := write(metadataBatesArtifactRecord{Type: metadataBatesArtifact, ArtifactID: artifact.ArtifactID,
			AllocationID: artifact.AllocationID, BlobSHA256: artifact.BlobSHA256, Size: artifact.Size,
			MediaType: artifact.MediaType, PageCount: artifact.PageCount, RecipeJSON: artifact.RecipeJSON,
			ManifestSHA256: artifact.ManifestSHA256, State: artifact.State, CreatedAt: artifact.CreatedAt}); err != nil {
			return err
		}
		for _, page := range artifact.Pages {
			if err := write(metadataBatesArtifactPageRecord{Type: metadataBatesArtifactPage, ArtifactID: id,
				OccurrenceID: page.OccurrenceID, SourceBlobSHA256: page.SourceBlobSHA256, Label: page.Label,
				Ordinal: page.Ordinal, SourcePage: page.SourcePage, OutputPage: page.OutputPage}); err != nil {
				return err
			}
		}
	}
	return nil
}

func exportBatesLabels(ctx context.Context, q metadataQuerier, write metadataWrite, allocationID string) error {
	rows, err := q.QueryContext(ctx, `SELECT ordinal,namespace_id,sequence,occurrence_id,source_page,output_page,label
		FROM bates_page_labels WHERE allocation_id=? ORDER BY ordinal`, allocationID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var label metadataBatesPageLabelRecord
		label.Type = metadataBatesPageLabel
		label.AllocationID = allocationID
		if err := rows.Scan(&label.Ordinal, &label.NamespaceID, &label.Sequence, &label.OccurrenceID,
			&label.SourcePage, &label.OutputPage, &label.Label); err != nil {
			return err
		}
		if err := write(label); err != nil {
			return err
		}
	}
	return rows.Err()
}

// batesMetadataTables registers the Bates ledger records for import. Their
// exporters interleave each namespace, allocation and artifact with its rows.
var batesMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataBatesNamespaceRecord]{record: metadataBatesNamespaceRecord{Type: metadataBatesNamespace}, table: "bates_namespaces", validate: validateBatesNamespaceRecord}),
	newMetadataTable(metadataTable[metadataBatesCursorRecord]{record: metadataBatesCursorRecord{Type: metadataBatesNamespaceCursor}, table: "bates_namespace_cursors", validate: validateBatesCursorRecord}),
	newMetadataTable(metadataTable[metadataBatesAllocationRecord]{record: metadataBatesAllocationRecord{Type: metadataBatesAllocation}, table: "bates_allocations", validate: validateBatesAllocationRecord}),
	newMetadataTable(metadataTable[metadataBatesPageLabelRecord]{record: metadataBatesPageLabelRecord{Type: metadataBatesPageLabel}, table: "bates_page_labels", validate: validateBatesPageLabelRecord}),
	newMetadataTable(metadataTable[metadataBatesArtifactRecord]{record: metadataBatesArtifactRecord{Type: metadataBatesArtifact}, table: "bates_artifacts", validate: validateBatesArtifactRecord}),
	newMetadataTable(metadataTable[metadataBatesArtifactPageRecord]{record: metadataBatesArtifactPageRecord{Type: metadataBatesArtifactPage}, table: "bates_artifact_pages", validate: validateBatesArtifactPageRecord}),
}

func validateBatesNamespaceRecord(r metadataBatesNamespaceRecord) error {
	if r.Type != metadataBatesNamespace || validateUUIDv4(r.NamespaceID) != nil || r.Padding < 1 || r.Padding > maxBatesPadding ||
		pdfstamp.ValidateLabelPart("prefix", r.Prefix) != nil || pdfstamp.ValidateLabelPart("suffix", r.Suffix) != nil || validateMetadataTime("Bates namespace", r.CreatedAt) != nil {
		return invalidBatesRecord(metadataBatesNamespace, r.NamespaceID)
	}
	return nil
}

func validateBatesCursorRecord(r metadataBatesCursorRecord) error {
	if r.Type != metadataBatesNamespaceCursor || validateUUIDv4(r.NamespaceID) != nil || r.NextSequence < 1 {
		return invalidBatesRecord(metadataBatesNamespaceCursor, r.NamespaceID)
	}
	return nil
}

func validateBatesAllocationRecord(r metadataBatesAllocationRecord) error {
	if r.Type != metadataBatesAllocation || validateUUIDv4(r.AllocationID) != nil || validateUUIDv4(r.OperationID) != nil ||
		validateUUIDv4(r.NamespaceID) != nil || validateUUIDv4(r.SnapshotID) != nil ||
		!canonical.IsSHA256Hex(r.RequestSHA256) || !canonical.IsSHA256Hex(r.RecipeSHA256) ||
		r.StartSequence < 1 || r.EndSequence < r.StartSequence || r.EndSequence-r.StartSequence >= MaxBatesExportPages ||
		validateMetadataTime("Bates allocation", r.CreatedAt) != nil ||
		(r.State != batesAllocationStateReserved && r.State != batesAllocationStateCommitted) ||
		(r.State == batesAllocationStateCommitted) != (r.CommittedAt != nil) {
		return invalidBatesRecord(metadataBatesAllocation, r.AllocationID)
	}
	if r.CommittedAt != nil && validateMetadataTime("Bates committed at", *r.CommittedAt) != nil {
		return invalidBatesRecord(metadataBatesAllocation, r.AllocationID)
	}
	return nil
}

func validateBatesPageLabelRecord(r metadataBatesPageLabelRecord) error {
	if r.Type != metadataBatesPageLabel || validateUUIDv4(r.AllocationID) != nil || validateUUIDv4(r.NamespaceID) != nil ||
		r.Ordinal < 1 || r.Ordinal > MaxBatesExportPages || r.Sequence < 1 || r.OccurrenceID == "" || r.SourcePage < 1 ||
		r.OutputPage < 1 || r.Label == "" {
		return invalidBatesRecord(metadataBatesPageLabel, fmt.Sprintf("%s/%d", r.AllocationID, r.Ordinal))
	}
	return nil
}

func validateBatesArtifactRecord(r metadataBatesArtifactRecord) error {
	if r.Type != metadataBatesArtifact || validateUUIDv4(r.ArtifactID) != nil || validateUUIDv4(r.AllocationID) != nil ||
		!canonical.IsSHA256Hex(r.BlobSHA256) || !canonical.IsSHA256Hex(r.ManifestSHA256) || r.Size < 1 ||
		r.MediaType != batesArtifactMediaTypePDF || r.PageCount < 1 || r.PageCount > MaxBatesExportPages || r.State != batesArtifactStateVerified ||
		len(r.RecipeJSON) == 0 || validateMetadataTime("Bates artifact", r.CreatedAt) != nil {
		return invalidBatesRecord(metadataBatesArtifact, r.ArtifactID)
	}
	return nil
}

func validateBatesArtifactPageRecord(r metadataBatesArtifactPageRecord) error {
	if r.Type != metadataBatesArtifactPage || validateUUIDv4(r.ArtifactID) != nil || r.Ordinal < 1 || r.Ordinal > MaxBatesExportPages || r.OccurrenceID == "" ||
		!canonical.IsSHA256Hex(r.SourceBlobSHA256) || r.SourcePage < 1 || r.OutputPage < 1 || r.Label == "" {
		return invalidBatesRecord(metadataBatesArtifactPage, fmt.Sprintf("%s/%d", r.ArtifactID, r.Ordinal))
	}
	return nil
}

func invalidBatesRecord(kind, id string) error {
	return fmt.Errorf("%w: %s record %s has invalid fields", ErrInvalidBatesLedger, kind, id)
}

func validateBatesMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := exportBatesMetadata(ctx, q, func(any) error { return nil }); err != nil {
		return err
	}
	namespaceIDs, err := pageMetadataKeys(ctx, q, `SELECT namespace_id FROM bates_namespaces ORDER BY namespace_id`)
	if err != nil {
		return err
	}
	for _, id := range namespaceIDs {
		var cursor, maxEnd int64
		var prefix, suffix string
		var padding int
		if err := q.QueryRowContext(ctx, `SELECT c.next_sequence,n.prefix,n.suffix,n.padding FROM bates_namespace_cursors c JOIN bates_namespaces n USING(namespace_id) WHERE c.namespace_id=?`, id).
			Scan(&cursor, &prefix, &suffix, &padding); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(end_sequence),0) FROM bates_allocations WHERE namespace_id=?`, id).Scan(&maxEnd); err != nil {
			return err
		}
		if cursor > batesMaxSequence(padding)+1 {
			return fmt.Errorf("%w: namespace %s cursor %d exceeds its %d-digit padding", ErrInvalidBatesLedger, id, cursor, padding)
		}
		if cursor <= maxEnd {
			return fmt.Errorf("%w: namespace %s cursor %d does not follow allocated sequence %d",
				ErrInvalidBatesLedger, id, cursor, maxEnd)
		}
		if err := validateBatesNamespaceLabels(ctx, q, id, prefix, suffix, padding); err != nil {
			return err
		}
	}
	if err := validateBatesCommitsHaveArtifacts(ctx, q); err != nil {
		return err
	}
	allocationIDs, err := pageMetadataKeys(ctx, q, `SELECT allocation_id FROM bates_allocations ORDER BY allocation_id`)
	if err != nil {
		return err
	}
	snapshotPages := map[string][]BatesPageInput{}
	for _, id := range allocationIDs {
		allocation, err := loadBatesAllocation(ctx, q, id)
		if err != nil {
			return err
		}
		expected, ok := snapshotPages[allocation.SnapshotID]
		if !ok {
			expected, err = snapshotSelectedPages(ctx, q, allocation.SnapshotID)
			if err != nil {
				return fmt.Errorf("validating Bates allocation %s snapshot: %w", id, err)
			}
			snapshotPages[allocation.SnapshotID] = expected
		}
		if len(allocation.Labels) > MaxBatesExportPages {
			return fmt.Errorf("%w: allocation %s has %d labels; the limit is %d",
				ErrInvalidBatesLedger, id, len(allocation.Labels), MaxBatesExportPages)
		}
		if len(expected) != len(allocation.Labels) {
			return fmt.Errorf("%w: allocation %s has %d labels for %d sealed pages",
				ErrInvalidBatesLedger, id, len(allocation.Labels), len(expected))
		}
		for i, page := range expected {
			label := allocation.Labels[i]
			if label.OccurrenceID != page.OccurrenceID || label.SourcePage != page.SourcePage {
				return fmt.Errorf("%w: allocation %s label %d differs from the sealed page order",
					ErrInvalidBatesLedger, id, i+1)
			}
		}
	}
	return nil
}

// validateBatesCommitsHaveArtifacts requires a committed range to have its
// verified export and a reserved range to have none, because publication
// records both in one transaction.
func validateBatesCommitsHaveArtifacts(ctx context.Context, q metadataQuerier) error {
	var allocationID, state string
	err := q.QueryRowContext(ctx, `SELECT a.allocation_id,a.state FROM bates_allocations a
		LEFT JOIN bates_artifacts r USING(allocation_id)
		WHERE (a.state=? AND r.artifact_id IS NULL) OR (a.state<>? AND r.artifact_id IS NOT NULL)
		ORDER BY a.allocation_id LIMIT 1`, batesAllocationStateCommitted, batesAllocationStateCommitted).
		Scan(&allocationID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("validating Bates commits: %w", err)
	}
	return fmt.Errorf("%w: %s allocation %s does not match its export record", ErrInvalidBatesLedger, state, allocationID)
}

// snapshotSelectedPages reads only the sealed page order and source PDF hashes,
// and refuses sources too large to stamp. Restore validation
// must not depend on page documents, which can be derived after reservation.
func snapshotSelectedPages(ctx context.Context, q metadataQuerier, snapshotID string) ([]BatesPageInput, error) {
	var pages []BatesPageInput
	for after := 0; ; {
		members, err := loadSnapshotMemberRows(ctx, q, snapshotID, after, 250)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			return pages, nil
		}
		for _, member := range members {
			if len(member.SelectedSourcePages) > 0 {
				if err := checkBatesSourceSize(ctx, q, member.SelectedPDFSHA256); err != nil {
					return nil, err
				}
			}
			for _, page := range member.SelectedSourcePages {
				pages = append(pages, BatesPageInput{OccurrenceID: member.OccurrenceID,
					UnstampedSHA256: member.SelectedPDFSHA256, SourcePage: page})
			}
			after = member.Ordinal
		}
	}
}

func validateBatesNamespaceLabels(ctx context.Context, q metadataQuerier, namespaceID, prefix, suffix string, padding int) error {
	rows, err := q.QueryContext(ctx, `SELECT a.allocation_id,a.start_sequence,a.end_sequence,l.ordinal,l.sequence,l.occurrence_id,
		l.source_page,l.output_page,l.label,l.namespace_id FROM bates_allocations a LEFT JOIN bates_page_labels l ON l.allocation_id=a.allocation_id
		WHERE a.namespace_id=? ORDER BY a.start_sequence,l.ordinal`, namespaceID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var lastEnd int64
	var allocationID string
	var ordinal int
	var expectedEnd int64
	for rows.Next() {
		var aid string
		var start, end int64
		var labelOrdinal, sourcePage, outputPage sql.NullInt64
		var sequence sql.NullInt64
		var occurrence, label, labelNamespace sql.NullString
		if err := rows.Scan(&aid, &start, &end, &labelOrdinal, &sequence, &occurrence, &sourcePage, &outputPage, &label, &labelNamespace); err != nil {
			return err
		}
		if aid != allocationID {
			if allocationID != "" && int64(ordinal) != expectedEnd {
				return batesLabelCountError(allocationID, ordinal, expectedEnd)
			}
			if end > batesMaxSequence(padding) {
				return fmt.Errorf("%w: allocation %s ends past its namespace's %d-digit padding", ErrInvalidBatesLedger, aid, padding)
			}
			if start <= lastEnd {
				return fmt.Errorf("%w: allocation %s overlaps an earlier range in namespace %s", ErrInvalidBatesLedger, aid, namespaceID)
			}
			allocationID = aid
			lastEnd = end
			expectedEnd = end - start + 1
			ordinal = 0
		}
		ordinal++
		if !labelOrdinal.Valid || labelOrdinal.Int64 != int64(ordinal) || sequence.Int64 != start+int64(ordinal)-1 ||
			labelNamespace.String != namespaceID || outputPage.Int64 != int64(ordinal) || sourcePage.Int64 < 1 ||
			label.String != fmt.Sprintf("%s%0*d%s", prefix, padding, sequence.Int64, suffix) {
			return fmt.Errorf("%w: allocation %s label %d does not match its namespace sequence", ErrInvalidBatesLedger, aid, ordinal)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if allocationID != "" && int64(ordinal) != expectedEnd {
		return batesLabelCountError(allocationID, ordinal, expectedEnd)
	}
	return nil
}

func batesLabelCountError(allocationID string, labels int, rangeSize int64) error {
	return fmt.Errorf("%w: allocation %s has %d labels for a %d-number range", ErrInvalidBatesLedger, allocationID, labels, rangeSize)
}
