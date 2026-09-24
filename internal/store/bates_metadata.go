package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
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
	NamespaceID string `json:"namespace_id"`
	Prefix      string `json:"prefix"`
	Suffix      string `json:"suffix"`
	Padding     int    `json:"padding"`
	CreatedAt   string `json:"created_at"`
}
type metadataBatesCursorRecord struct {
	Type         string `json:"type"`
	NamespaceID  string `json:"namespace_id"`
	NextSequence int64  `json:"next_sequence"`
}
type metadataBatesAllocationRecord struct {
	Type          string  `json:"type"`
	AllocationID  string  `json:"allocation_id"`
	OperationID   string  `json:"operation_id"`
	NamespaceID   string  `json:"namespace_id"`
	SnapshotID    string  `json:"snapshot_id"`
	RequestSHA256 string  `json:"request_sha256"`
	RecipeSHA256  string  `json:"recipe_sha256"`
	StartSequence int64   `json:"start_sequence"`
	EndSequence   int64   `json:"end_sequence"`
	State         string  `json:"state"`
	CreatedAt     string  `json:"created_at"`
	CommittedAt   *string `json:"committed_at"`
}
type metadataBatesPageLabelRecord struct {
	Type         string `json:"type"`
	AllocationID string `json:"allocation_id"`
	Ordinal      int    `json:"ordinal"`
	NamespaceID  string `json:"namespace_id"`
	Sequence     int64  `json:"sequence"`
	OccurrenceID string `json:"occurrence_id"`
	SourcePage   int    `json:"source_page"`
	OutputPage   int    `json:"output_page"`
	Label        string `json:"label"`
}
type metadataBatesArtifactRecord struct {
	Type           string         `json:"type"`
	ArtifactID     string         `json:"artifact_id"`
	AllocationID   string         `json:"allocation_id"`
	BlobSHA256     string         `json:"blob_sha256"`
	Size           int64          `json:"size"`
	MediaType      string         `json:"media_type"`
	PageCount      int            `json:"page_count"`
	RecipeJSON     jsontext.Value `json:"recipe_json"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	State          string         `json:"state"`
	CreatedAt      string         `json:"created_at"`
}
type metadataBatesArtifactPageRecord struct {
	Type             string `json:"type"`
	ArtifactID       string `json:"artifact_id"`
	Ordinal          int    `json:"ordinal"`
	OccurrenceID     string `json:"occurrence_id"`
	SourceBlobSHA256 string `json:"source_blob_sha256"`
	SourcePage       int    `json:"source_page"`
	OutputPage       int    `json:"output_page"`
	Label            string `json:"label"`
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

func importBatesMetadata(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataBatesNamespace:
		var r metadataBatesNamespaceRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.NamespaceID) != nil || r.Padding < 1 || r.Padding > maxBatesPadding ||
			!validBatesLabelPart(r.Prefix) || !validBatesLabelPart(r.Suffix) || validateMetadataTime("Bates namespace", r.CreatedAt) != nil {
			return invalidBatesRecord(kind, r.NamespaceID)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_namespaces(namespace_id,prefix,suffix,padding,created_at) VALUES(?,?,?,?,?)`, r.NamespaceID, r.Prefix, r.Suffix, r.Padding, r.CreatedAt)
		return err
	case metadataBatesNamespaceCursor:
		var r metadataBatesCursorRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.NamespaceID) != nil || r.NextSequence < 1 {
			return invalidBatesRecord(kind, r.NamespaceID)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_namespace_cursors(namespace_id,next_sequence) VALUES(?,?)`, r.NamespaceID, r.NextSequence)
		return err
	case metadataBatesAllocation:
		var r metadataBatesAllocationRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.AllocationID) != nil || validateUUIDv4(r.OperationID) != nil ||
			validateUUIDv4(r.NamespaceID) != nil || validateUUIDv4(r.SnapshotID) != nil ||
			!canonical.IsSHA256Hex(r.RequestSHA256) || !canonical.IsSHA256Hex(r.RecipeSHA256) ||
			r.StartSequence < 1 || r.EndSequence < r.StartSequence || validateMetadataTime("Bates allocation", r.CreatedAt) != nil ||
			(r.State != batesAllocationStateReserved && r.State != batesAllocationStateCommitted) ||
			(r.State == batesAllocationStateCommitted) != (r.CommittedAt != nil) {
			return invalidBatesRecord(kind, r.AllocationID)
		}
		if r.CommittedAt != nil && validateMetadataTime("Bates committed at", *r.CommittedAt) != nil {
			return invalidBatesRecord(kind, r.AllocationID)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_allocations(allocation_id,operation_id,namespace_id,snapshot_id,request_sha256,
			recipe_sha256,start_sequence,end_sequence,state,created_at,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.AllocationID,
			r.OperationID, r.NamespaceID, r.SnapshotID, r.RequestSHA256, r.RecipeSHA256, r.StartSequence, r.EndSequence, r.State,
			r.CreatedAt, r.CommittedAt)
		return err
	case metadataBatesPageLabel:
		var r metadataBatesPageLabelRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.AllocationID) != nil || validateUUIDv4(r.NamespaceID) != nil ||
			r.Ordinal < 1 || r.Sequence < 1 || r.OccurrenceID == "" || r.SourcePage < 1 || r.OutputPage < 1 || r.Label == "" {
			return invalidBatesRecord(kind, fmt.Sprintf("%s/%d", r.AllocationID, r.Ordinal))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_page_labels(allocation_id,ordinal,namespace_id,sequence,occurrence_id,
			source_page,output_page,label) VALUES(?,?,?,?,?,?,?,?)`, r.AllocationID, r.Ordinal, r.NamespaceID, r.Sequence,
			r.OccurrenceID, r.SourcePage, r.OutputPage, r.Label)
		return err
	case metadataBatesArtifact:
		var r metadataBatesArtifactRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.ArtifactID) != nil || validateUUIDv4(r.AllocationID) != nil ||
			!canonical.IsSHA256Hex(r.BlobSHA256) || !canonical.IsSHA256Hex(r.ManifestSHA256) || r.Size < 1 ||
			r.MediaType != batesArtifactMediaTypePDF || r.PageCount < 1 || r.State != batesArtifactStateVerified ||
			len(r.RecipeJSON) == 0 || validateMetadataTime("Bates artifact", r.CreatedAt) != nil {
			return invalidBatesRecord(kind, r.ArtifactID)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_artifacts(artifact_id,allocation_id,blob_hash,size,media_type,page_count,recipe_json,manifest_sha256,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, r.ArtifactID, r.AllocationID, r.BlobSHA256, r.Size, r.MediaType,
			r.PageCount, []byte(r.RecipeJSON), r.ManifestSHA256, r.State, r.CreatedAt)
		return err
	case metadataBatesArtifactPage:
		var r metadataBatesArtifactPageRecord
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if r.Type != kind || validateUUIDv4(r.ArtifactID) != nil || r.Ordinal < 1 || r.OccurrenceID == "" ||
			!canonical.IsSHA256Hex(r.SourceBlobSHA256) || r.SourcePage < 1 || r.OutputPage < 1 || r.Label == "" {
			return invalidBatesRecord(kind, fmt.Sprintf("%s/%d", r.ArtifactID, r.Ordinal))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bates_artifact_pages(artifact_id,ordinal,occurrence_id,source_blob_sha256,source_page,output_page,label)
			VALUES(?,?,?,?,?,?,?)`, r.ArtifactID, r.Ordinal, r.OccurrenceID, r.SourceBlobSHA256, r.SourcePage, r.OutputPage, r.Label)
		return err
	}
	return fmt.Errorf("%w: unknown record type %q", ErrInvalidBatesLedger, kind)
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

// snapshotSelectedPages reads only the sealed page order and source PDF hashes. Restore validation
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
