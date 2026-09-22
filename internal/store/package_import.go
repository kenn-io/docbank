package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

var ErrAmbiguousLabel = errors.New("ambiguous_label: label matches more than one scoped identity")

const packageReceiptRejected = "rejected"

type PackageRecordRow struct {
	PackageID    string `json:"package_id"`
	RowID        string `json:"row_id"`
	LoadFile     string `json:"load_file"`
	RowOrdinal   int    `json:"row_ordinal"`
	OccurrenceID string `json:"occurrence_id"`
	RawJSON      []byte `json:"raw_json" format:"byte"`
	RawSHA256    string `json:"raw_sha256"`
	Sensitive    bool   `json:"sensitive"`
}

type PackageLabelRow struct {
	PackageID        string `json:"package_id"`
	Provenance       string `json:"provenance"`
	LabelSet         string `json:"label_set"`
	Label            string `json:"label"`
	LabelSortKey     string `json:"label_sort_key"`
	OccurrenceID     string `json:"occurrence_id"`
	ContentVersionID string `json:"content_version_id"`
	ArtifactID       string `json:"artifact_id"`
	PageNumber       int    `json:"page_number"`
	PageState        string `json:"page_state"`
	Endpoint         string `json:"endpoint"`
}

type PackageImportReceipt struct {
	ReceiptID        string `json:"receipt_id"`
	PackageID        string `json:"package_id"`
	RecordKey        string `json:"record_key"`
	OccurrenceID     string `json:"occurrence_id"`
	ContentVersionID string `json:"content_version_id"`
	State            string `json:"state"`
	ReceiptJSON      []byte `json:"receipt_json" format:"byte"`
	RecordedAt       string `json:"recorded_at"`
}

// PackageRecordKey binds every source coordinate without delimiter ambiguity.
func PackageRecordKey(loadFile string, rowOrdinal int, docID string) (string, error) {
	if loadFile == "" || docID == "" || rowOrdinal < 1 || len(loadFile) > 4096 ||
		len(docID) > 65536 || !utf8.ValidString(loadFile) || !utf8.ValidString(docID) {
		return "", ErrPackageConflict
	}
	raw, err := canonical.Marshal([]any{"package-record/v1", loadFile, rowOrdinal, docID})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// PackageOccurrenceID keeps repeated source bytes as separate occurrences.
func PackageOccurrenceID(packageID, recordKey string) string {
	raw, err := canonical.Marshal([]string{packageID, recordKey})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:16])
}

// LabelSortKey preserves sender spelling; assigned numeric order lives in the ledger.
func LabelSortKey(label string) string { return label }

func validatePackageRecord(record PackageRecordRow) error {
	if validateUUIDv4(record.PackageID) != nil || !canonical.IsSHA256Hex(record.RowID) ||
		record.LoadFile == "" || len(record.LoadFile) > 4096 || !utf8.ValidString(record.LoadFile) ||
		record.RowOrdinal < 1 || len(record.OccurrenceID) != 32 ||
		len(record.RawJSON) == 0 || len(record.RawJSON) > 1<<20 || !canonical.IsSHA256Hex(record.RawSHA256) {
		return ErrPackageConflict
	}
	for _, char := range record.OccurrenceID {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return ErrPackageConflict
		}
	}
	digest := sha256.Sum256(record.RawJSON)
	if hex.EncodeToString(digest[:]) != record.RawSHA256 ||
		PackageOccurrenceID(record.PackageID, record.RowID) != record.OccurrenceID {
		return ErrPackageConflict
	}
	return nil
}

func validatePackageLabel(label PackageLabelRow) error {
	if validateUUIDv4(label.PackageID) != nil ||
		!slices.Contains([]string{packageDirectionReceived, "assigned"}, label.Provenance) ||
		label.LabelSet == "" || len(label.LabelSet) > 256 || !utf8.ValidString(label.LabelSet) ||
		label.Label == "" || len(label.Label) > 256 || !utf8.ValidString(label.Label) ||
		label.LabelSortKey != LabelSortKey(label.Label) || label.OccurrenceID == "" ||
		validateUUIDv4(label.ContentVersionID) != nil || label.PageNumber < 0 ||
		!slices.Contains([]string{"unknown", "verified"}, label.PageState) ||
		!slices.Contains([]string{"begin", "end", "page", "begin_attach", "end_attach"}, label.Endpoint) {
		return ErrPackageConflict
	}
	if label.Endpoint == "page" && label.PageNumber == 0 || label.Endpoint != "page" && label.PageNumber != 0 {
		return ErrPackageConflict
	}
	return nil
}

func validatePackageImportReceipt(receipt PackageImportReceipt) error {
	if validateUUIDv4(receipt.ReceiptID) != nil || validateUUIDv4(receipt.PackageID) != nil ||
		!canonical.IsSHA256Hex(receipt.RecordKey) || receipt.OccurrenceID != PackageOccurrenceID(receipt.PackageID, receipt.RecordKey) ||
		receipt.ContentVersionID != "" && validateUUIDv4(receipt.ContentVersionID) != nil ||
		!slices.Contains([]string{"committed", packageReceiptRejected, "skipped"}, receipt.State) ||
		len(receipt.ReceiptJSON) == 0 || len(receipt.ReceiptJSON) > 1<<20 {
		return ErrPackageConflict
	}
	if receipt.RecordedAt != "" && validateMetadataTime("package receipt recorded_at", receipt.RecordedAt) != nil {
		return ErrPackageConflict
	}
	return nil
}

func loadPackageRecordTx(ctx context.Context, tx metadataQuerier, packageID, rowID string) (PackageRecordRow, error) {
	var record PackageRecordRow
	err := tx.QueryRowContext(ctx, `SELECT package_id,row_id,load_file,row_ordinal,occurrence_id,
		raw_json,raw_sha256,sensitive FROM package_records WHERE package_id=? AND row_id=?`, packageID, rowID).Scan(
		&record.PackageID, &record.RowID, &record.LoadFile, &record.RowOrdinal,
		&record.OccurrenceID, &record.RawJSON, &record.RawSHA256, &record.Sensitive)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageRecordRow{}, ErrNotFound
	}
	return record, err
}

func loadPackageLabelsTx(ctx context.Context, tx metadataQuerier, packageID, occurrenceID string) ([]PackageLabelRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT package_id,provenance,label_set,label,label_sort_key,
		occurrence_id,content_version_id,COALESCE(artifact_id,''),COALESCE(page_number,0),page_state,endpoint
		FROM package_labels WHERE package_id=? AND occurrence_id=? AND provenance='received'
		ORDER BY provenance,label_set,label,endpoint,occurrence_id`, packageID, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var labels []PackageLabelRow
	for rows.Next() {
		var label PackageLabelRow
		if err := rows.Scan(&label.PackageID, &label.Provenance, &label.LabelSet, &label.Label,
			&label.LabelSortKey, &label.OccurrenceID, &label.ContentVersionID, &label.ArtifactID,
			&label.PageNumber, &label.PageState, &label.Endpoint); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func loadPackageImportHeadTx(ctx context.Context, tx metadataQuerier, packageID, key string) (PackageImportReceipt, error) {
	var receipt PackageImportReceipt
	err := tx.QueryRowContext(ctx, `SELECT r.receipt_id,r.package_id,r.record_key,r.occurrence_id,
		COALESCE(r.content_version_id,''),r.state,r.receipt_json,r.recorded_at
		FROM package_import_heads h JOIN package_import_receipts r ON r.receipt_id=h.receipt_id
		WHERE h.package_id=? AND h.record_key=?`, packageID, key).Scan(
		&receipt.ReceiptID, &receipt.PackageID, &receipt.RecordKey, &receipt.OccurrenceID,
		&receipt.ContentVersionID, &receipt.State, &receipt.ReceiptJSON, &receipt.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageImportReceipt{}, ErrNotFound
	}
	return receipt, err
}

type packageImportLease struct {
	jobID string
	epoch int64
	token string
}

// CommitPackageRecordWithLease rejects publication from a cancelled, expired,
// or replaced worker before any row or receipt changes.
func (s *Store) CommitPackageRecordWithLease(ctx context.Context, jobID string, epoch int64, token string,
	record PackageRecordRow, labels []PackageLabelRow, receipt PackageImportReceipt) (PackageImportReceipt, error) {
	if validateUUIDv4(jobID) != nil || receipt.State != "committed" {
		return PackageImportReceipt{}, ErrPackageConflict
	}
	return s.commitPackageRecord(ctx, record, labels, receipt, packageImportLease{jobID, epoch, token})
}

// RecordPackageGapWithLease preserves a declared but unfulfilled row without
// inventing a content version or publishing a sender label against absent bytes.
func (s *Store) RecordPackageGapWithLease(ctx context.Context, jobID string, epoch int64, token string,
	record PackageRecordRow, receipt PackageImportReceipt) (PackageImportReceipt, error) {
	if validateUUIDv4(jobID) != nil || receipt.State != packageReceiptRejected && receipt.State != "skipped" {
		return PackageImportReceipt{}, ErrPackageConflict
	}
	return s.commitPackageRecord(ctx, record, nil, receipt, packageImportLease{jobID, epoch, token})
}

func (s *Store) commitPackageRecord(ctx context.Context, record PackageRecordRow, labels []PackageLabelRow,
	receipt PackageImportReceipt, lease packageImportLease) (PackageImportReceipt, error) {
	if validatePackageRecord(record) != nil || validatePackageImportReceipt(receipt) != nil ||
		receipt.PackageID != record.PackageID || receipt.RecordKey != record.RowID ||
		receipt.OccurrenceID != record.OccurrenceID ||
		(receipt.State == "committed") != (receipt.ContentVersionID != "") ||
		(receipt.State != "committed" && len(labels) != 0) || len(labels) > 1_000_000 {
		return PackageImportReceipt{}, ErrPackageConflict
	}
	labels = slices.Clone(labels)
	for _, label := range labels {
		if validatePackageLabel(label) != nil || label.PackageID != record.PackageID ||
			label.OccurrenceID != record.OccurrenceID || label.ContentVersionID != receipt.ContentVersionID ||
			label.Provenance != packageDirectionReceived {
			return PackageImportReceipt{}, ErrPackageConflict
		}
	}
	slices.SortFunc(labels, comparePackageLabel)
	for index := 1; index < len(labels); index++ {
		if comparePackageLabel(labels[index-1], labels[index]) == 0 {
			return PackageImportReceipt{}, ErrPackageConflict
		}
	}
	var result PackageImportReceipt
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		job, err := packageImportLeaseHeld(ctx, tx, lease.jobID, lease.epoch, lease.token)
		if err != nil {
			return err
		}
		if job.PackageID != record.PackageID {
			return ErrPackageConflict
		}
		pkg, err := loadPackageTx(ctx, tx, record.PackageID)
		if err != nil {
			return err
		}
		if pkg.Direction != packageDirectionReceived {
			return ErrPackageConflict
		}
		existing, err := loadPackageImportHeadTx(ctx, tx, record.PackageID, record.RowID)
		if err == nil {
			stored, err := loadPackageRecordTx(ctx, tx, record.PackageID, record.RowID)
			if err != nil {
				return err
			}
			storedLabels, err := loadPackageLabelsTx(ctx, tx, record.PackageID, record.OccurrenceID)
			if err != nil {
				return err
			}
			if stored.PackageID != record.PackageID || stored.RowID != record.RowID ||
				stored.LoadFile != record.LoadFile || stored.RowOrdinal != record.RowOrdinal ||
				stored.OccurrenceID != record.OccurrenceID || stored.RawSHA256 != record.RawSHA256 ||
				stored.Sensitive != record.Sensitive || !bytes.Equal(stored.RawJSON, record.RawJSON) ||
				!slices.Equal(storedLabels, labels) || existing.State != receipt.State ||
				existing.ContentVersionID != receipt.ContentVersionID ||
				!bytes.Equal(existing.ReceiptJSON, receipt.ReceiptJSON) {
				return ErrPackageConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if pkg.State != "importing" {
			return ErrPackageConflict
		}
		if receipt.State == "committed" {
			var bound bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM provenance_version_bindings b
				JOIN provenance p ON p.identity=b.provenance_identity
				WHERE b.content_version_id=? AND p.ingest_id=?)`, receipt.ContentVersionID, pkg.IngestID).Scan(&bound); err != nil {
				return err
			}
			if !bound {
				return ErrPackageConflict
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_records(package_id,row_id,load_file,row_ordinal,
			occurrence_id,raw_json,raw_sha256,sensitive) VALUES(?,?,?,?,?,?,?,?)`, record.PackageID,
			record.RowID, record.LoadFile, record.RowOrdinal, record.OccurrenceID, record.RawJSON,
			record.RawSHA256, record.Sensitive)
		if err != nil {
			return err
		}
		for _, label := range labels {
			_, err := tx.ExecContext(ctx, `INSERT INTO package_labels(package_id,provenance,label_set,label,
				label_sort_key,occurrence_id,content_version_id,artifact_id,page_number,page_state,endpoint)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, label.PackageID, label.Provenance, label.LabelSet, label.Label,
				label.LabelSortKey, label.OccurrenceID, label.ContentVersionID, nullableString(label.ArtifactID),
				packagePageNullable(label.PageNumber), label.PageState, label.Endpoint)
			if err != nil {
				return err
			}
		}
		if receipt.RecordedAt == "" {
			receipt.RecordedAt = nowRFC3339()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_import_receipts(receipt_id,package_id,record_key,
			occurrence_id,content_version_id,state,receipt_json,recorded_at) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.ReceiptID, receipt.PackageID, receipt.RecordKey, receipt.OccurrenceID,
			nullableString(receipt.ContentVersionID), receipt.State, receipt.ReceiptJSON, receipt.RecordedAt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_import_heads(package_id,record_key,receipt_id)
			VALUES(?,?,?)`, receipt.PackageID, receipt.RecordKey, receipt.ReceiptID)
		if err != nil {
			return err
		}
		result = receipt
		return nil
	})
	return result, err
}

func comparePackageLabel(left, right PackageLabelRow) int {
	for _, pair := range [][2]string{{left.Provenance, right.Provenance}, {left.LabelSet, right.LabelSet},
		{left.Label, right.Label}, {left.Endpoint, right.Endpoint}, {left.OccurrenceID, right.OccurrenceID}} {
		if order := strings.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	return 0
}

// AssignPackageLabels publishes the complete assigned-label set for one
// imported occurrence. Exact replay succeeds; later divergence is rejected so
// received sender labels and assigned production labels remain separate.
func (s *Store) AssignPackageLabels(ctx context.Context, packageID, occurrenceID, contentVersionID string, labels []PackageLabelRow) error {
	if validateUUIDv4(packageID) != nil || !validPackageOccurrenceID(occurrenceID) ||
		validateUUIDv4(contentVersionID) != nil || len(labels) == 0 || len(labels) > MaxSnapshotPages {
		return ErrPackageConflict
	}
	labels = slices.Clone(labels)
	for _, label := range labels {
		if validatePackageLabel(label) != nil || label.PackageID != packageID ||
			label.OccurrenceID != occurrenceID || label.ContentVersionID != contentVersionID ||
			label.Provenance != "assigned" {
			return ErrPackageConflict
		}
	}
	slices.SortFunc(labels, comparePackageLabel)
	for index := 1; index < len(labels); index++ {
		if comparePackageLabel(labels[index-1], labels[index]) == 0 {
			return ErrPackageConflict
		}
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var boundVersion string
		if err := tx.QueryRowContext(ctx, `SELECT r.content_version_id FROM package_import_heads h
			JOIN package_import_receipts r ON r.receipt_id=h.receipt_id
			WHERE h.package_id=? AND r.occurrence_id=?`, packageID, occurrenceID).Scan(&boundVersion); err != nil {
			return err
		}
		if boundVersion != contentVersionID {
			return ErrPackageConflict
		}
		existing, err := loadAssignedPackageLabels(ctx, tx, packageID, occurrenceID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			if !slices.Equal(existing, labels) {
				return ErrPackageConflict
			}
			return nil
		}
		for _, label := range labels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO package_labels(package_id,provenance,label_set,label,
				label_sort_key,occurrence_id,content_version_id,artifact_id,page_number,page_state,endpoint)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, label.PackageID, label.Provenance, label.LabelSet, label.Label,
				label.LabelSortKey, label.OccurrenceID, label.ContentVersionID, nullableString(label.ArtifactID),
				packagePageNullable(label.PageNumber), label.PageState, label.Endpoint); err != nil {
				return err
			}
		}
		return nil
	})
}

func loadAssignedPackageLabels(ctx context.Context, q metadataQuerier, packageID, occurrenceID string) ([]PackageLabelRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT package_id,provenance,label_set,label,label_sort_key,
		occurrence_id,content_version_id,COALESCE(artifact_id,''),COALESCE(page_number,0),page_state,endpoint
		FROM package_labels WHERE package_id=? AND occurrence_id=? AND provenance='assigned'
		ORDER BY provenance,label_set,label,endpoint,occurrence_id`, packageID, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var labels []PackageLabelRow
	for rows.Next() {
		var label PackageLabelRow
		if err := rows.Scan(&label.PackageID, &label.Provenance, &label.LabelSet, &label.Label,
			&label.LabelSortKey, &label.OccurrenceID, &label.ContentVersionID, &label.ArtifactID,
			&label.PageNumber, &label.PageState, &label.Endpoint); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func packagePageNullable(page int) any {
	if page == 0 {
		return nil
	}
	return page
}

func (s *Store) PackageImportHead(ctx context.Context, packageID, key string) (PackageImportReceipt, error) {
	if validateUUIDv4(packageID) != nil || !canonical.IsSHA256Hex(key) {
		return PackageImportReceipt{}, ErrPackageConflict
	}
	return loadPackageImportHeadTx(ctx, s.db, packageID, key)
}

func (s *Store) PackageRecord(ctx context.Context, packageID, rowID string) (PackageRecordRow, error) {
	if validateUUIDv4(packageID) != nil || !canonical.IsSHA256Hex(rowID) {
		return PackageRecordRow{}, ErrPackageConflict
	}
	return loadPackageRecordTx(ctx, s.db, packageID, rowID)
}

// LookupPackageLabel requires the caller to scope a reused sender label.
func (s *Store) LookupPackageLabel(ctx context.Context, label, packageID, labelSet, provenance string) ([]PackageLabelRow, error) {
	if label == "" || len(label) > 256 || packageID != "" && validateUUIDv4(packageID) != nil ||
		labelSet != "" && len(labelSet) > 256 ||
		provenance != "" && !slices.Contains([]string{packageDirectionReceived, "assigned"}, provenance) {
		return nil, ErrPackageConflict
	}
	rows, err := s.db.QueryContext(ctx, `SELECT package_id,provenance,label_set,label,label_sort_key,
		occurrence_id,content_version_id,COALESCE(artifact_id,''),COALESCE(page_number,0),page_state,endpoint
		FROM package_labels WHERE label=? AND (?='' OR package_id=?) AND (?='' OR label_set=?)
		AND (?='' OR provenance=?) ORDER BY package_id,occurrence_id,artifact_id,page_number,endpoint LIMIT 251`,
		label, packageID, packageID, labelSet, labelSet, provenance, provenance)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var labels []PackageLabelRow
	identities := make(map[string]struct{})
	for rows.Next() {
		var item PackageLabelRow
		if err := rows.Scan(&item.PackageID, &item.Provenance, &item.LabelSet, &item.Label,
			&item.LabelSortKey, &item.OccurrenceID, &item.ContentVersionID, &item.ArtifactID,
			&item.PageNumber, &item.PageState, &item.Endpoint); err != nil {
			return nil, err
		}
		labels = append(labels, item)
		// A sender may use the same label for a document endpoint and its
		// first page. They still resolve to one received occurrence.
		identities[item.PackageID+"/"+item.OccurrenceID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(labels) > 250 {
		return nil, ErrPackageConflict
	}
	if len(identities) > 1 {
		var packages []string
		for _, item := range labels {
			packages = append(packages, item.PackageID)
		}
		slices.Sort(packages)
		packages = slices.Compact(packages)
		return nil, fmt.Errorf("%w: packages %s", ErrAmbiguousLabel, strings.Join(packages, ","))
	}
	return labels, nil
}
