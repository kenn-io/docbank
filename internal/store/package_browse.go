package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

type packageLabelCursor struct {
	Label            string `json:"label"`
	FilterPackageID  string `json:"filter_package_id"`
	FilterLabelSet   string `json:"filter_label_set"`
	FilterProvenance string `json:"filter_provenance"`
	PackageID        string `json:"package_id"`
	Provenance       string `json:"provenance"`
	LabelSet         string `json:"label_set"`
	OccurrenceID     string `json:"occurrence_id"`
	ArtifactID       string `json:"artifact_id"`
	PageNumber       int    `json:"page_number"`
	Endpoint         string `json:"endpoint"`
}

// PackageTimelineInput retains the sender's exact row bytes beside the package
// timezone declaration and production date. Callers parse claims explicitly.
type PackageTimelineInput struct {
	PackageID        string `json:"package_id"`
	OccurrenceID     string `json:"occurrence_id"`
	RowID            string `json:"row_id"`
	RawJSON          []byte `json:"raw_json" format:"byte"`
	DeclaredTimezone string `json:"declared_timezone"`
	ProducedOn       string `json:"produced_on"`
}

// PackageMembers pages the immutable snapshot selected by a package.
func (s *Store) PackageMembers(ctx context.Context, packageID string, afterOrdinal, limit int) ([]CollectionSnapshotMember, error) {
	if validateUUIDv4(packageID) != nil || afterOrdinal < 0 || limit < 1 || limit > 250 {
		return nil, ErrPackageConflict
	}
	var snapshotID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_id FROM packages WHERE package_id=?`, packageID).Scan(&snapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !snapshotID.Valid || snapshotID.String == "" {
		return nil, nil
	}
	return s.SnapshotMembers(ctx, snapshotID.String, afterOrdinal, limit)
}

// PackageRecordRowIDs maps each occurrence on one member page to its retained
// row key. Occurrences without a retained row are absent from the result.
func (s *Store) PackageRecordRowIDs(ctx context.Context, packageID string, occurrenceIDs []string) (map[string]string, error) {
	if validateUUIDv4(packageID) != nil || len(occurrenceIDs) > 250 {
		return nil, ErrPackageConflict
	}
	result := make(map[string]string, len(occurrenceIDs))
	if len(occurrenceIDs) == 0 {
		return result, nil
	}
	args := make([]any, 0, len(occurrenceIDs)+1)
	args = append(args, packageID)
	for _, occurrenceID := range occurrenceIDs {
		if !validPackageOccurrenceID(occurrenceID) {
			return nil, ErrPackageConflict
		}
		args = append(args, occurrenceID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT occurrence_id,row_id FROM package_records
		WHERE package_id=? AND occurrence_id IN (`+placeholders(len(occurrenceIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var occurrenceID, rowID string
		if err := rows.Scan(&occurrenceID, &rowID); err != nil {
			return nil, err
		}
		result[occurrenceID] = rowID
	}
	return result, rows.Err()
}

// PackageRecordByOccurrence resolves the opaque record key used by package
// browser members without treating a received label as a global identifier.
func (s *Store) PackageRecordByOccurrence(ctx context.Context, packageID, occurrenceID string) (PackageRecordRow, error) {
	if validateUUIDv4(packageID) != nil || !validPackageOccurrenceID(occurrenceID) {
		return PackageRecordRow{}, ErrPackageConflict
	}
	var rowID string
	err := s.db.QueryRowContext(ctx, `SELECT row_id FROM package_records WHERE package_id=? AND occurrence_id=?`,
		packageID, occurrenceID).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageRecordRow{}, ErrNotFound
	}
	if err != nil {
		return PackageRecordRow{}, err
	}
	return loadPackageRecordTx(ctx, s.db, packageID, rowID)
}

// PackageLabels returns every received and assigned label for one occurrence.
func (s *Store) PackageLabels(ctx context.Context, packageID, occurrenceID string) ([]PackageLabelRow, error) {
	if validateUUIDv4(packageID) != nil || !validPackageOccurrenceID(occurrenceID) {
		return nil, ErrPackageConflict
	}
	rows, err := s.db.QueryContext(ctx, `SELECT package_id,provenance,label_set,label,label_sort_key,
		occurrence_id,content_version_id,COALESCE(artifact_id,''),COALESCE(page_number,0),page_state,endpoint
		FROM package_labels WHERE package_id=? AND occurrence_id=?
		ORDER BY package_id,provenance,label_set,occurrence_id,COALESCE(artifact_id,''),COALESCE(page_number,0),endpoint`,
		packageID, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PackageLabelRow, 0)
	for rows.Next() {
		var item PackageLabelRow
		if err := rows.Scan(&item.PackageID, &item.Provenance, &item.LabelSet, &item.Label,
			&item.LabelSortKey, &item.OccurrenceID, &item.ContentVersionID, &item.ArtifactID,
			&item.PageNumber, &item.PageState, &item.Endpoint); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func validPackageOccurrenceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// PackageTimelineInputs returns retained rows in stable source order.
func (s *Store) PackageTimelineInputs(ctx context.Context, packageID, afterRowID string, limit int) ([]PackageTimelineInput, error) {
	if validateUUIDv4(packageID) != nil || afterRowID != "" && !canonical.IsSHA256Hex(afterRowID) || limit < 1 || limit > 250 {
		return nil, ErrPackageConflict
	}
	var profileJSON []byte
	var producedOn string
	err := s.db.QueryRowContext(ctx, `SELECT profile_json,COALESCE(produced_on,'') FROM packages WHERE package_id=?`, packageID).
		Scan(&profileJSON, &producedOn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	profile, err := canonical.Decode[loadfile.Profile](profileJSON)
	if err != nil {
		return nil, ErrPackageConflict
	}
	afterOrdinal := 0
	if afterRowID != "" {
		err = s.db.QueryRowContext(ctx, `SELECT row_ordinal FROM package_records WHERE package_id=? AND row_id=?`,
			packageID, afterRowID).Scan(&afterOrdinal)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPackageConflict
		}
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT occurrence_id,row_id,raw_json FROM package_records
		WHERE package_id=? AND (row_ordinal>? OR (row_ordinal=? AND row_id>?))
		ORDER BY row_ordinal,row_id LIMIT ?`, packageID, afterOrdinal, afterOrdinal, afterRowID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	inputs := make([]PackageTimelineInput, 0)
	for rows.Next() {
		input := PackageTimelineInput{PackageID: packageID, DeclaredTimezone: profile.DeclaredTimezone, ProducedOn: producedOn}
		if err := rows.Scan(&input.OccurrenceID, &input.RowID, &input.RawJSON); err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

// PackageLabelCandidates returns a bounded stable page without collapsing
// reused sender labels. The opaque cursor is safe to pass directly to an API.
func (s *Store) PackageLabelCandidates(ctx context.Context, label, packageID, labelSet, provenance, after string, limit int) ([]PackageLabelRow, string, error) {
	if label == "" || len(label) > 256 || packageID != "" && validateUUIDv4(packageID) != nil ||
		labelSet != "" && len(labelSet) > 256 ||
		provenance != "" && !slices.Contains([]string{packageDirectionReceived, "assigned"}, provenance) ||
		limit < 1 || limit > 250 {
		return nil, "", ErrPackageConflict
	}
	var cursor packageLabelCursor
	if after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			return nil, "", ErrPackageConflict
		}
		cursor, err = canonical.Decode[packageLabelCursor](raw)
		if err != nil || validateUUIDv4(cursor.PackageID) != nil || cursor.OccurrenceID == "" ||
			cursor.Label != label || cursor.FilterPackageID != packageID ||
			cursor.FilterLabelSet != labelSet || cursor.FilterProvenance != provenance {
			return nil, "", ErrPackageConflict
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT package_id,provenance,label_set,label,label_sort_key,
		occurrence_id,content_version_id,COALESCE(artifact_id,''),COALESCE(page_number,0),page_state,endpoint
		FROM package_labels WHERE label=? AND (?='' OR package_id=?) AND (?='' OR label_set=?)
		AND (?='' OR provenance=?) AND (?='' OR (package_id,provenance,label_set,occurrence_id,
		COALESCE(artifact_id,''),COALESCE(page_number,0),endpoint)>(?,?,?,?,?,?,?))
		ORDER BY package_id,provenance,label_set,occurrence_id,COALESCE(artifact_id,''),COALESCE(page_number,0),endpoint
		LIMIT ?`, label, packageID, packageID, labelSet, labelSet, provenance, provenance,
		after, cursor.PackageID, cursor.Provenance, cursor.LabelSet, cursor.OccurrenceID,
		cursor.ArtifactID, cursor.PageNumber, cursor.Endpoint, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PackageLabelRow, 0, limit+1)
	for rows.Next() {
		var item PackageLabelRow
		if err := rows.Scan(&item.PackageID, &item.Provenance, &item.LabelSet, &item.Label,
			&item.LabelSortKey, &item.OccurrenceID, &item.ContentVersionID, &item.ArtifactID,
			&item.PageNumber, &item.PageState, &item.Endpoint); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	encoded, err := canonical.Marshal(packageLabelCursor{Label: label, FilterPackageID: packageID,
		FilterLabelSet: labelSet, FilterProvenance: provenance, PackageID: last.PackageID,
		Provenance: last.Provenance, LabelSet: last.LabelSet, OccurrenceID: last.OccurrenceID,
		ArtifactID: last.ArtifactID, PageNumber: last.PageNumber, Endpoint: last.Endpoint})
	if err != nil {
		return nil, "", err
	}
	return items, base64.RawURLEncoding.EncodeToString(encoded), nil
}
