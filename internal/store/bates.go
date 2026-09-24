package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

type BatesNamespace struct {
	NamespaceID, Prefix, Suffix, CreatedAt string
	Padding                                int
}
type BatesPageLabel struct {
	Ordinal                int
	OccurrenceID           string
	SourcePage, OutputPage int
	Label                  string
}
type BatesPageInput struct {
	OccurrenceID, UnstampedSHA256 string
	SourcePage, VerifiedPageCount int
}
type BatesPlanRequest struct {
	OperationID, NamespaceID, SnapshotID, RecipeSHA256 string
	StartAt                                            int64
	Pages                                              []BatesPageInput
}
type BatesAllocation struct {
	AllocationID, NamespaceID, SnapshotID, RequestSHA256, RecipeSHA256, State, CreatedAt, CommittedAt string
	StartSequence, EndSequence                                                                        int64
	Labels                                                                                            []BatesPageLabel
}
type BatesPlan struct {
	Namespace     BatesNamespace
	StartSequence int64
	EndSequence   int64
	Labels        []BatesPageLabel
}

// MaxBatesExportPages bounds one Bates allocation and its stamped PDF.
const MaxBatesExportPages = 250

var (
	ErrBatesReservationConflict = errors.New("bates_reservation_conflict: concurrent or divergent reservation")
	ErrBatesOverflow            = errors.New("bates_overflow: the range exceeds the namespace padding")
	ErrBatesPageCountMismatch   = errors.New("bates_page_count_mismatch: verified page counts differ from the sealed plan")
	ErrBatesPageLimit           = fmt.Errorf("bates_page_limit: a Bates export holds at most %d pages", MaxBatesExportPages)
	ErrBatesLabelCollision      = errors.New("bates_label_collision: a label in this range is already allocated by another namespace")
	ErrInvalidBatesRequest      = errors.New("invalid_bates_request")
	ErrInvalidBatesCursor       = errors.New("invalid Bates export history cursor")
	// ErrInvalidBatesLedger reports restored or audited Bates rows that break
	// the reservation ledger's invariants.
	ErrInvalidBatesLedger = errors.New("invalid Bates ledger")
)

const (
	batesAllocationStateReserved  = "reserved"
	batesAllocationStateCommitted = "committed"
	batesArtifactStateVerified    = "verified"
	batesArtifactMediaTypePDF     = "application/pdf"
	maxBatesPadding               = 10
)

// batesMaxSequence is the largest number that fits the padding, 10^padding-1.
func batesMaxSequence(padding int) int64 {
	maxValue := int64(1)
	for range padding {
		maxValue *= 10
	}
	return maxValue - 1
}

func invalidBatesRequest(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidBatesRequest, reason)
}

func batesLabel(namespace BatesNamespace, sequence int64) string {
	return fmt.Sprintf("%s%0*d%s", namespace.Prefix, namespace.Padding, sequence, namespace.Suffix)
}

// batesLabelCollision reports whether any label in [start,end] is already
// allocated. Labels are globally unique, so distinct namespaces such as
// prefix "A" padding 7 and prefix "A1" padding 6 can produce the same text.
func batesLabelCollision(ctx context.Context, q metadataQuerier, namespace BatesNamespace, start, end int64) error {
	labels := make([]string, 0, end-start+1)
	for sequence := start; sequence <= end; sequence++ {
		labels = append(labels, batesLabel(namespace, sequence))
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("encoding Bates labels: %w", err)
	}
	var existing string
	err = q.QueryRowContext(ctx, `SELECT label FROM bates_page_labels
		WHERE label IN (SELECT value FROM json_each(?)) LIMIT 1`, string(encoded)).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking Bates label collisions: %w", err)
	}
	return fmt.Errorf("%w: %s", ErrBatesLabelCollision, existing)
}

func batesRange(cursor, startAt, pages int64, padding int) (int64, int64, error) {
	if padding < 1 || padding > maxBatesPadding || pages < 1 || cursor < 1 || startAt < 0 {
		return 0, 0, ErrBatesOverflow
	}
	start := cursor
	if startAt != 0 {
		if startAt < cursor {
			return 0, 0, ErrBatesReservationConflict
		}
		start = startAt
	}
	maxValue := batesMaxSequence(padding)
	if start > maxValue || pages-1 > maxValue-start {
		return 0, 0, ErrBatesOverflow
	}
	return start, start + pages - 1, nil
}

// PreviewBatesRange reads pinned page and cursor authority without allocating labels.
func (s *Store) PreviewBatesRange(ctx context.Context, r BatesPlanRequest) (BatesPlan, error) {
	if validateUUIDv4(r.NamespaceID) != nil || validateUUIDv4(r.SnapshotID) != nil {
		return BatesPlan{}, invalidBatesRequest("namespace_id and snapshot_id must be UUIDs")
	}
	if r.StartAt < 0 {
		return BatesPlan{}, invalidBatesRequest("start_at must be nonnegative")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatesPlan{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var plan BatesPlan
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT n.namespace_id,n.prefix,n.suffix,n.padding,n.created_at,c.next_sequence
		FROM bates_namespaces n JOIN bates_namespace_cursors c USING(namespace_id) WHERE n.namespace_id=?`, r.NamespaceID).
		Scan(&plan.Namespace.NamespaceID, &plan.Namespace.Prefix, &plan.Namespace.Suffix, &plan.Namespace.Padding, &plan.Namespace.CreatedAt, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return BatesPlan{}, ErrNotFound
	}
	if err != nil {
		return BatesPlan{}, err
	}
	if err := validateBatesPages(ctx, tx, r); err != nil {
		return BatesPlan{}, err
	}
	plan.StartSequence, plan.EndSequence, err = batesRange(cursor, r.StartAt, int64(len(r.Pages)), plan.Namespace.Padding)
	if err != nil {
		return BatesPlan{}, err
	}
	if err := batesLabelCollision(ctx, tx, plan.Namespace, plan.StartSequence, plan.EndSequence); err != nil {
		return BatesPlan{}, err
	}
	for i, page := range r.Pages {
		label := batesLabel(plan.Namespace, plan.StartSequence+int64(i))
		plan.Labels = append(plan.Labels, BatesPageLabel{i + 1, page.OccurrenceID, page.SourcePage, i + 1, label})
	}
	return plan, tx.Commit()
}

func (s *Store) EnsureBatesNamespace(ctx context.Context, prefix, suffix string, padding int) (BatesNamespace, error) {
	if padding < 1 || padding > maxBatesPadding {
		return BatesNamespace{}, invalidBatesRequest(fmt.Sprintf("padding must be between 1 and %d", maxBatesPadding))
	}
	if !validBatesLabelPart(prefix) || !validBatesLabelPart(suffix) {
		return BatesNamespace{}, invalidBatesRequest("prefix and suffix must be printable ASCII without % or \\")
	}
	var result BatesNamespace
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT namespace_id,prefix,suffix,padding,created_at FROM bates_namespaces WHERE prefix=? AND suffix=?`, prefix, suffix).
			Scan(&result.NamespaceID, &result.Prefix, &result.Suffix, &result.Padding, &result.CreatedAt)
		if err == nil {
			if result.Padding != padding {
				result = BatesNamespace{}
				return ErrBatesReservationConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		result = BatesNamespace{id, prefix, suffix, nowRFC3339(), padding}
		_, err = tx.ExecContext(ctx, `INSERT INTO bates_namespaces(namespace_id,prefix,suffix,padding,created_at) VALUES(?,?,?,?,?)`,
			result.NamespaceID, result.Prefix, result.Suffix, result.Padding, result.CreatedAt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bates_namespace_cursors(namespace_id,next_sequence) VALUES(?,1)`, id)
		return err
	})
	if err != nil {
		return BatesNamespace{}, err
	}
	return result, err
}

// BatesNamespaces returns a stable, bounded page ordered by namespace ID.
func (s *Store) BatesNamespaces(ctx context.Context, after string, limit int) ([]BatesNamespace, int, string, error) {
	if limit < 1 || limit > 250 {
		return nil, 0, "", invalidBatesRequest("limit must be between 1 and 250")
	}
	if after != "" && validateUUIDv4(after) != nil {
		return nil, 0, "", ErrInvalidBatesCursor
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_namespaces`).Scan(&total); err != nil {
		return nil, 0, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT namespace_id,prefix,suffix,padding,created_at FROM bates_namespaces
		WHERE namespace_id>? ORDER BY namespace_id LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = rows.Close() }()
	items := make([]BatesNamespace, 0, limit)
	for rows.Next() {
		var value BatesNamespace
		if err := rows.Scan(&value.NamespaceID, &value.Prefix, &value.Suffix, &value.Padding, &value.CreatedAt); err != nil {
			return nil, 0, "", err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	var next string
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].NamespaceID
	}
	return items, total, next, nil
}

func (s *Store) BatesAllocation(ctx context.Context, id string) (BatesAllocation, error) {
	if validateUUIDv4(id) != nil {
		return BatesAllocation{}, ErrNotFound
	}
	return loadBatesAllocation(ctx, s.db, id)
}

func (s *Store) BatesNamespace(ctx context.Context, id, prefix, suffix string, padding int) (BatesNamespace, error) {
	if id != "" && validateUUIDv4(id) != nil {
		return BatesNamespace{}, ErrNotFound
	}
	var value BatesNamespace
	var err error
	if id != "" {
		err = s.db.QueryRowContext(ctx, `SELECT namespace_id,prefix,suffix,padding,created_at FROM bates_namespaces WHERE namespace_id=?`, id).
			Scan(&value.NamespaceID, &value.Prefix, &value.Suffix, &value.Padding, &value.CreatedAt)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT namespace_id,prefix,suffix,padding,created_at FROM bates_namespaces WHERE prefix=? AND suffix=?`, prefix, suffix).
			Scan(&value.NamespaceID, &value.Prefix, &value.Suffix, &value.Padding, &value.CreatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return BatesNamespace{}, ErrNotFound
	}
	if err != nil {
		return BatesNamespace{}, err
	}
	if prefix != "" && value.Prefix != prefix || suffix != "" && value.Suffix != suffix || padding != 0 && value.Padding != padding {
		return BatesNamespace{}, ErrBatesReservationConflict
	}
	return value, nil
}

func batesRequestDigest(r BatesPlanRequest) (string, error) {
	encoded, err := canonical.Marshal(struct {
		NamespaceID  string           `json:"namespace_id"`
		SnapshotID   string           `json:"snapshot_id"`
		RecipeSHA256 string           `json:"recipe_sha256"`
		StartAt      int64            `json:"start_at"`
		Pages        []BatesPageInput `json:"pages"`
	}{r.NamespaceID, r.SnapshotID, r.RecipeSHA256, r.StartAt, r.Pages})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateBatesPages(ctx context.Context, tx *sql.Tx, r BatesPlanRequest) error {
	if len(r.Pages) > MaxBatesExportPages {
		return ErrBatesPageLimit
	}
	if len(r.Pages) == 0 {
		return ErrBatesPageCountMismatch
	}
	expected, err := expectedBatesPagesLimited(ctx, tx, r.SnapshotID, MaxBatesExportPages)
	if err != nil {
		return err
	}
	if len(expected) != len(r.Pages) {
		return ErrBatesPageCountMismatch
	}
	for i, page := range expected {
		if page != r.Pages[i] || !canonical.IsSHA256Hex(r.Pages[i].UnstampedSHA256) {
			return ErrBatesPageCountMismatch
		}
	}
	return nil
}

// SnapshotBatesPages reads the exact sealed source-page order of a snapshot
// small enough for one Bates export.
func (s *Store) SnapshotBatesPages(ctx context.Context, snapshotID string) ([]BatesPageInput, error) {
	if validateUUIDv4(snapshotID) != nil {
		return nil, invalidBatesRequest("snapshot_id must be a UUID")
	}
	return expectedBatesPagesLimited(ctx, s.db, snapshotID, MaxBatesExportPages)
}

func expectedBatesPagesLimited(ctx context.Context, tx metadataQuerier, snapshotID string, limit int) ([]BatesPageInput, error) {
	var expected []BatesPageInput
	for after := 0; ; {
		members, err := loadSnapshotMemberRows(ctx, tx, snapshotID, after, 250)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			break
		}
		for _, member := range members {
			member.Representations, err = loadSnapshotRepresentationRows(ctx, tx, snapshotID, member.OccurrenceID)
			if err != nil {
				return nil, err
			}
			if member.SourcePageCount < 1 || member.SelectedPDFSHA256 == "" {
				return nil, ErrBatesPageCountMismatch
			}
			pageDoc, err := loadPageDocument(ctx, tx, member.ContentVersionID)
			if errors.Is(err, ErrNotFound) && snapshotPDFRepresentationValid(member) {
				err = nil
			}
			if err != nil || pageDoc.PageCount != 0 && (pageDoc.PageCount != member.SourcePageCount || pageDoc.Source.SHA256 != member.SelectedPDFSHA256) {
				return nil, ErrBatesPageCountMismatch
			}
			for _, page := range member.SelectedSourcePages {
				expected = append(expected, BatesPageInput{member.OccurrenceID, member.SelectedPDFSHA256, page, pageDoc.PageCount})
				if len(expected) > limit {
					return nil, ErrBatesPageLimit
				}
			}
			after = member.Ordinal
		}
	}
	return expected, nil
}

func (s *Store) ReserveBatesRange(ctx context.Context, r BatesPlanRequest) (BatesAllocation, error) {
	if validateUUIDv4(r.OperationID) != nil || validateUUIDv4(r.NamespaceID) != nil || validateUUIDv4(r.SnapshotID) != nil {
		return BatesAllocation{}, invalidBatesRequest("operation_id, namespace_id, and snapshot_id must be UUIDs")
	}
	if !canonical.IsSHA256Hex(r.RecipeSHA256) {
		return BatesAllocation{}, invalidBatesRequest("recipe_sha256 must be a lowercase SHA-256 digest")
	}
	// The recipe digest already fixes its start_at, so the reservation must
	// name the same explicit start instead of following a moving cursor.
	if r.StartAt < 1 {
		return BatesAllocation{}, invalidBatesRequest("start_at must be the recipe's first Bates number")
	}
	digest, err := batesRequestDigest(r)
	if err != nil {
		return BatesAllocation{}, err
	}
	var allocation BatesAllocation
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var priorDigest, priorID string
		err := tx.QueryRowContext(ctx, `SELECT allocation_id,request_sha256 FROM bates_allocations WHERE operation_id=?`, r.OperationID).Scan(&priorID, &priorDigest)
		if err == nil {
			if priorDigest != digest {
				return ErrBatesReservationConflict
			}
			allocation, err = loadBatesAllocation(ctx, tx, priorID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var namespace BatesNamespace
		var cursor int64
		err = tx.QueryRowContext(ctx, `SELECT n.namespace_id,n.prefix,n.suffix,n.padding,n.created_at,c.next_sequence
			FROM bates_namespaces n JOIN bates_namespace_cursors c USING(namespace_id) WHERE n.namespace_id=?`, r.NamespaceID).
			Scan(&namespace.NamespaceID, &namespace.Prefix, &namespace.Suffix, &namespace.Padding, &namespace.CreatedAt, &cursor)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := validateBatesPages(ctx, tx, r); err != nil {
			return err
		}
		start, end, err := batesRange(cursor, r.StartAt, int64(len(r.Pages)), namespace.Padding)
		if err != nil {
			return err
		}
		if err := batesLabelCollision(ctx, tx, namespace, start, end); err != nil {
			return err
		}
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		allocation = BatesAllocation{AllocationID: id, NamespaceID: r.NamespaceID, SnapshotID: r.SnapshotID,
			RequestSHA256: digest, RecipeSHA256: r.RecipeSHA256, State: batesAllocationStateReserved, CreatedAt: nowRFC3339(),
			StartSequence: start, EndSequence: end}
		_, err = tx.ExecContext(ctx, `INSERT INTO bates_allocations(allocation_id,operation_id,namespace_id,snapshot_id,request_sha256,
			recipe_sha256,start_sequence,end_sequence,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, r.OperationID, r.NamespaceID,
			r.SnapshotID, digest, r.RecipeSHA256, start, end, allocation.State, allocation.CreatedAt)
		if err != nil {
			return err
		}
		for i, page := range r.Pages {
			label := batesLabel(namespace, start+int64(i))
			_, err = tx.ExecContext(ctx, `INSERT INTO bates_page_labels(allocation_id,ordinal,namespace_id,sequence,occurrence_id,
				source_page,output_page,label) VALUES(?,?,?,?,?,?,?,?)`, id, i+1, r.NamespaceID, start+int64(i), page.OccurrenceID,
				page.SourcePage, i+1, label)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrBatesReservationConflict, err)
			}
			allocation.Labels = append(allocation.Labels, BatesPageLabel{i + 1, page.OccurrenceID, page.SourcePage, i + 1, label})
		}
		_, err = tx.ExecContext(ctx, `UPDATE bates_namespace_cursors SET next_sequence=? WHERE namespace_id=?`, end+1, r.NamespaceID)
		return err
	})
	if err != nil {
		return BatesAllocation{}, err
	}
	return allocation, err
}

func validBatesLabelPart(value string) bool {
	for _, character := range value {
		if character < 0x20 || character > 0x7e || character == '%' || character == '\\' {
			return false
		}
	}
	return true
}

func loadBatesAllocation(ctx context.Context, q metadataQuerier, id string) (BatesAllocation, error) {
	var a BatesAllocation
	var committed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT allocation_id,namespace_id,snapshot_id,request_sha256,recipe_sha256,
		start_sequence,end_sequence,state,created_at,committed_at FROM bates_allocations WHERE allocation_id=?`, id).
		Scan(&a.AllocationID, &a.NamespaceID, &a.SnapshotID, &a.RequestSHA256, &a.RecipeSHA256,
			&a.StartSequence, &a.EndSequence, &a.State, &a.CreatedAt, &committed)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CommittedAt = committed.String
	rows, err := q.QueryContext(ctx, `SELECT ordinal,occurrence_id,source_page,output_page,label FROM bates_page_labels
		WHERE allocation_id=? ORDER BY ordinal`, id)
	if err != nil {
		return a, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var label BatesPageLabel
		if err := rows.Scan(&label.Ordinal, &label.OccurrenceID, &label.SourcePage, &label.OutputPage, &label.Label); err != nil {
			return a, err
		}
		a.Labels = append(a.Labels, label)
	}
	return a, rows.Err()
}
