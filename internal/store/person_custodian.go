package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

var ErrCustodianConflict = errors.New("custodian conflict")

type CustodianAssignment struct {
	AssignmentID, ScopeKind              string
	IngestID, PackageID, PackageRecordID *string
	NodeID                               *int64
	ContentVersionID, PersonID           *string
	RawLabel, Rank, Basis, SourceRef     string
	Revision                             int64
	RecordedAt                           string
	RetiredAt                            *string
}

type CustodianScope struct {
	Kind               string
	IngestID           string
	PackageID          string
	PackageRecordID    string
	HasPackageRecordID bool
	ContentVersionID   string
	NodeID             int64
}

type CustodianRequest struct {
	Scope                                      CustodianScope
	PersonID, RawLabel, Rank, Basis, SourceRef string
	IfMatchRevision                            int64
}

func validateCustodianScope(scope CustodianScope) error {
	switch scope.Kind {
	case "collection":
		if scope.IngestID != "" && scope.PackageID == "" && scope.PackageRecordID == "" && scope.ContentVersionID == "" && scope.NodeID == 0 {
			return nil
		}
	case "package":
		if scope.PackageID != "" && scope.IngestID == "" && scope.ContentVersionID == "" && scope.NodeID == 0 {
			return nil
		}
	case "document":
		if scope.ContentVersionID != "" && scope.NodeID > 0 && scope.IngestID == "" && scope.PackageID == "" && scope.PackageRecordID == "" {
			return nil
		}
	}
	return errors.New("invalid custodian scope coordinates")
}

func custodianPrecedence(assignment CustodianAssignment) int {
	switch {
	case assignment.ScopeKind == "document" && assignment.Basis == "operator_assigned":
		return 0
	case assignment.ScopeKind == "document" && assignment.Basis == "transfer_record":
		return 1
	case assignment.ScopeKind == "package" && assignment.PackageRecordID != nil && *assignment.PackageRecordID != "":
		return 2
	case assignment.ScopeKind == "package":
		return 3
	default:
		return 4
	}
}

func (s *Store) SetCustodian(ctx context.Context, request CustodianRequest) (CustodianAssignment, error) {
	if err := validateCustodianScope(request.Scope); err != nil || request.IfMatchRevision < 1 ||
		!utf8.ValidString(request.RawLabel) || strings.TrimSpace(request.RawLabel) == "" || len(request.RawLabel) > document.MaxPersonDisplayNameBytes ||
		!slices.Contains([]string{"primary", "additional"}, request.Rank) ||
		!slices.Contains([]string{"operator_assigned", "package_column", "transfer_record"}, request.Basis) ||
		len(request.SourceRef) > document.MaxCustodianSourceRefBytes {
		return CustodianAssignment{}, ErrInvalidPerson
	}
	var assignmentID string
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := validateCustodianReferencesTx(ctx, tx, request); err != nil {
			return err
		}
		predicate, args := exactCustodianScopePredicate(request.Scope)
		if request.Rank == "primary" {
			var revision int64
			err := tx.QueryRowContext(ctx, `SELECT assignment_id,revision FROM custodian_assignments WHERE retired_at IS NULL AND rank='primary' AND `+predicate, args...).Scan(&assignmentID, &revision)
			if err == nil {
				if revision != request.IfMatchRevision {
					return ErrStaleRevision
				}
				personID := nullableCustodianString(request.PersonID)
				result, err := tx.ExecContext(ctx, `UPDATE custodian_assignments SET person_id=?,raw_label=?,raw_label_folded=?,basis=?,source_ref=?,revision=revision+1,recorded_at=? WHERE assignment_id=? AND revision=?`, personID, request.RawLabel, document.FoldPersonName(request.RawLabel), request.Basis, request.SourceRef, nowRFC3339(), assignmentID, revision)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if changed != 1 {
					return ErrStaleRevision
				}
				return markCustodianScopeDirtyTx(ctx, tx, request.Scope, "custodian_updated")
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if request.IfMatchRevision != 1 {
			return ErrStaleRevision
		}
		var err error
		assignmentID, err = newUUIDv4()
		if err != nil {
			return err
		}
		ingestID, packageID, packageRecordID, nodeID, versionID := custodianScopeValues(request.Scope)
		_, err = tx.ExecContext(ctx, `INSERT INTO custodian_assignments(assignment_id,scope_kind,ingest_id,package_id,package_record_id,node_id,content_version_id,person_id,raw_label,raw_label_folded,rank,basis,source_ref,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, assignmentID, request.Scope.Kind, ingestID, packageID, packageRecordID, nodeID, versionID, nullableCustodianString(request.PersonID), request.RawLabel, document.FoldPersonName(request.RawLabel), request.Rank, request.Basis, request.SourceRef, nowRFC3339())
		if s.driver.IsUniqueViolation(err) {
			return ErrCustodianConflict
		}
		if err != nil {
			return err
		}
		return markCustodianScopeDirtyTx(ctx, tx, request.Scope, "custodian_added")
	})
	if err != nil {
		return CustodianAssignment{}, err
	}
	return s.custodianByID(ctx, assignmentID)
}

func nullableCustodianString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func custodianScopeValues(scope CustodianScope) (ingestID, packageID, packageRecordID, nodeID, versionID any) {
	switch scope.Kind {
	case "collection":
		return scope.IngestID, nil, nil, nil, nil
	case "package":
		return nil, scope.PackageID, scope.PackageRecordID, nil, nil
	default:
		return nil, nil, nil, scope.NodeID, scope.ContentVersionID
	}
}

func validateCustodianReferencesTx(ctx context.Context, tx *sql.Tx, request CustodianRequest) error {
	if request.PersonID != "" {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM persons WHERE person_id=?`, request.PersonID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		} else if state == "retired" {
			return ErrPersonRetired
		}
	}
	switch request.Scope.Kind {
	case "collection":
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ingests WHERE id=?)`, request.Scope.IngestID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	case "document":
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions WHERE version_id=? AND node_id=?)`, request.Scope.ContentVersionID, request.Scope.NodeID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func exactCustodianScopePredicate(scope CustodianScope) (string, []any) {
	switch scope.Kind {
	case "collection":
		return `scope_kind='collection' AND ingest_id=?`, []any{scope.IngestID}
	case "package":
		return `scope_kind='package' AND package_id=? AND package_record_id=?`, []any{scope.PackageID, scope.PackageRecordID}
	default:
		return `scope_kind='document' AND content_version_id=?`, []any{scope.ContentVersionID}
	}
}

func markCustodianScopeDirtyTx(ctx context.Context, tx *sql.Tx, scope CustodianScope, reason string) error {
	var query string
	var args []any
	switch scope.Kind {
	case "document":
		query, args = `SELECT version_id FROM content_versions WHERE version_id=? AND node_id=?`, []any{scope.ContentVersionID, scope.NodeID}
	case "collection":
		query, args = `WITH `+CollectionMembershipCTE+` SELECT DISTINCT n.current_version_id FROM collection_members cm JOIN nodes n ON n.id=cm.node_id WHERE cm.ingest_id=? AND n.current_version_id IS NOT NULL`, []any{scope.IngestID}
	}
	if query != "" {
		versions, err := custodianScopeVersions(ctx, tx, query, args)
		if err != nil {
			return err
		}
		if len(versions) <= document.MaxPersonDirtyVersionsPerScan {
			for _, version := range versions {
				if _, err := tx.ExecContext(ctx, `INSERT INTO document_people_dirty(content_version_id,reason,marked_at) VALUES(?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET revision=revision+1,reason=excluded.reason,marked_at=excluded.marked_at`, version, reason, nowRFC3339()); err != nil {
					return err
				}
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1,updated_at=? WHERE singleton=1`, nowRFC3339())
	return err
}

func custodianScopeVersions(ctx context.Context, tx *sql.Tx, query string, args []any) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	versions := make([]string, 0)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
		if len(versions) > document.MaxPersonDirtyVersionsPerScan {
			return versions, nil
		}
	}
	return versions, rows.Err()
}

func (s *Store) RetireCustodian(ctx context.Context, assignmentID string, revision int64) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		assignment, err := custodianByIDTx(ctx, tx, assignmentID)
		if err != nil {
			return err
		}
		if assignment.RetiredAt != nil {
			return ErrCustodianConflict
		}
		if assignment.Revision != revision {
			return ErrStaleRevision
		}
		now := nowRFC3339()
		result, err := tx.ExecContext(ctx, `UPDATE custodian_assignments SET retired_at=?,revision=revision+1 WHERE assignment_id=? AND revision=?`, now, assignmentID, revision)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrStaleRevision
		}
		return markCustodianScopeDirtyTx(ctx, tx, custodianAssignmentScope(assignment), "custodian_retired")
	})
}

const custodianColumns = `assignment_id,scope_kind,ingest_id,package_id,package_record_id,node_id,content_version_id,person_id,raw_label,rank,basis,source_ref,revision,recorded_at,retired_at`

func scanCustodian(row interface{ Scan(dest ...any) error }) (CustodianAssignment, error) {
	var assignment CustodianAssignment
	var ingestID, packageID, packageRecordID, contentVersionID, personID, retiredAt sql.NullString
	var nodeID sql.NullInt64
	err := row.Scan(&assignment.AssignmentID, &assignment.ScopeKind, &ingestID, &packageID, &packageRecordID, &nodeID, &contentVersionID, &personID, &assignment.RawLabel, &assignment.Rank, &assignment.Basis, &assignment.SourceRef, &assignment.Revision, &assignment.RecordedAt, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CustodianAssignment{}, ErrNotFound
	}
	if err != nil {
		return CustodianAssignment{}, err
	}
	assignment.IngestID, assignment.PackageID, assignment.PackageRecordID = stringPtr(ingestID), stringPtr(packageID), stringPtr(packageRecordID)
	assignment.ContentVersionID, assignment.PersonID, assignment.RetiredAt = stringPtr(contentVersionID), stringPtr(personID), stringPtr(retiredAt)
	if nodeID.Valid {
		assignment.NodeID = &nodeID.Int64
	}
	return assignment, nil
}

func custodianByIDTx(ctx context.Context, tx *sql.Tx, assignmentID string) (CustodianAssignment, error) {
	return scanCustodian(tx.QueryRowContext(ctx, `SELECT `+custodianColumns+` FROM custodian_assignments WHERE assignment_id=?`, assignmentID))
}

func (s *Store) custodianByID(ctx context.Context, assignmentID string) (CustodianAssignment, error) {
	return scanCustodian(s.db.QueryRowContext(ctx, `SELECT `+custodianColumns+` FROM custodian_assignments WHERE assignment_id=?`, assignmentID))
}

func custodianAssignmentScope(assignment CustodianAssignment) CustodianScope {
	scope := CustodianScope{Kind: assignment.ScopeKind}
	if assignment.IngestID != nil {
		scope.IngestID = *assignment.IngestID
	}
	if assignment.PackageID != nil {
		scope.PackageID = *assignment.PackageID
	}
	if assignment.PackageRecordID != nil {
		scope.PackageRecordID, scope.HasPackageRecordID = *assignment.PackageRecordID, true
	}
	if assignment.NodeID != nil {
		scope.NodeID = *assignment.NodeID
	}
	if assignment.ContentVersionID != nil {
		scope.ContentVersionID = *assignment.ContentVersionID
	}
	return scope
}

func (s *Store) CustodiansForVersion(ctx context.Context, contentVersionID string) ([]CustodianAssignment, error) {
	rows, err := s.db.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT `+custodianColumns+` FROM custodian_assignments ca
		WHERE ca.retired_at IS NULL AND (
			(ca.scope_kind='document' AND ca.content_version_id=?) OR
			(ca.scope_kind='collection' AND EXISTS(SELECT 1 FROM collection_members cm JOIN nodes n ON n.id=cm.node_id WHERE cm.ingest_id=ca.ingest_id AND n.current_version_id=?)))
		ORDER BY CASE WHEN ca.scope_kind='document' AND ca.basis='operator_assigned' THEN 0 WHEN ca.scope_kind='document' AND ca.basis='transfer_record' THEN 1 ELSE 4 END,ca.recorded_at,ca.assignment_id`, contentVersionID, contentVersionID)
	if err != nil {
		return nil, err
	}
	return scanCustodianRows(rows)
}

func scanCustodianRows(rows *sql.Rows) (_ []CustodianAssignment, retErr error) {
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	assignments := []CustodianAssignment{}
	for rows.Next() {
		assignment, err := scanCustodian(rows)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

func (s *Store) Custodians(ctx context.Context, scope CustodianScope, unresolvedOnly bool, limit, offset int) ([]CustodianAssignment, int64, error) {
	if scope.Kind != "" {
		if scope.Kind == "package" && !scope.HasPackageRecordID {
			if scope.PackageID == "" || scope.PackageRecordID != "" || scope.IngestID != "" || scope.ContentVersionID != "" || scope.NodeID != 0 {
				return nil, 0, ErrInvalidPerson
			}
		} else if err := validateCustodianScope(scope); err != nil {
			return nil, 0, err
		}
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 250 || offset < 0 {
		return nil, 0, ErrInvalidPerson
	}
	where := []string{"retired_at IS NULL"}
	args := []any{}
	if unresolvedOnly {
		where = append(where, "person_id IS NULL")
	}
	if scope.Kind != "" {
		where = append(where, "scope_kind=?")
		args = append(args, scope.Kind)
		switch scope.Kind {
		case "collection":
			where = append(where, "ingest_id=?")
			args = append(args, scope.IngestID)
		case "package":
			where = append(where, "package_id=?")
			args = append(args, scope.PackageID)
			if scope.HasPackageRecordID {
				where = append(where, "package_record_id=?")
				args = append(args, scope.PackageRecordID)
			}
		case "document":
			where = append(where, "content_version_id=? AND node_id=?")
			args = append(args, scope.ContentVersionID, scope.NodeID)
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	clause := strings.Join(where, " AND ")
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM custodian_assignments WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(slices.Clone(args), limit, offset)
	rows, err := tx.QueryContext(ctx, `SELECT `+custodianColumns+` FROM custodian_assignments WHERE `+clause+` ORDER BY recorded_at,assignment_id LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	items, err := scanCustodianRows(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
