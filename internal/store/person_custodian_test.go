package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustodianScopeRejectsMixedCoordinates(t *testing.T) {
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a", PackageID: "package-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a"}))
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a", NodeID: 2}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "package", PackageRecordID: "record-a"}))
}

func TestCustodianAssignmentUsesRealVersion(t *testing.T) {
	s := newTestStore(t)
	node, version := seedPeopleVersion(t, s)
	_, err := s.SetCustodian(t.Context(), CustodianRequest{Scope: CustodianScope{
		Kind: "document", NodeID: node, ContentVersionID: version}, RawLabel: "Records Team",
		Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)
	rows, err := s.CustodiansForVersion(t.Context(), version)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Records Team", rows[0].RawLabel)
	require.Nil(t, rows[0].PersonID)
}

func TestCustodianMutationUsesRevisionFenceAndNullableCoordinates(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, version := seedPeopleVersion(t, s)
	first, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(ctx, "Grace Hopper", "operator")
	require.NoError(t, err)
	scope := CustodianScope{Kind: "document", NodeID: node, ContentVersionID: version}
	var epochBefore int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epochBefore))

	assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, PersonID: first.PersonID,
		RawLabel: "Ada Lovelace", Rank: "primary", Basis: "operator_assigned", SourceRef: "operator", IfMatchRevision: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, assignment.Revision)
	require.Nil(t, assignment.IngestID)
	require.Nil(t, assignment.PackageID)
	require.Nil(t, assignment.PackageRecordID)
	require.NotNil(t, assignment.NodeID)
	require.Equal(t, node, *assignment.NodeID)
	require.NotNil(t, assignment.ContentVersionID)
	require.Equal(t, version, *assignment.ContentVersionID)
	require.NotNil(t, assignment.PersonID)
	require.Equal(t, first.PersonID, *assignment.PersonID)
	var folded string
	require.NoError(t, s.db.QueryRow(`SELECT raw_label_folded FROM custodian_assignments WHERE assignment_id=?`, assignment.AssignmentID).Scan(&folded))
	require.Equal(t, "ada lovelace", folded)

	_, err = s.SetCustodian(ctx, CustodianRequest{Scope: scope, PersonID: second.PersonID,
		RawLabel: "Grace Hopper", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 2})
	require.ErrorIs(t, err, ErrStaleRevision)
	updated, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, PersonID: second.PersonID,
		RawLabel: "Grace Hopper", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)
	require.Equal(t, assignment.AssignmentID, updated.AssignmentID)
	require.EqualValues(t, 2, updated.Revision)

	require.ErrorIs(t, s.RetireCustodian(ctx, updated.AssignmentID, 1), ErrStaleRevision)
	require.NoError(t, s.RetireCustodian(ctx, updated.AssignmentID, 2))
	rows, err := s.CustodiansForVersion(ctx, version)
	require.NoError(t, err)
	require.Empty(t, rows)
	var dirty, epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM document_people_dirty WHERE content_version_id=?`, version).Scan(&dirty))
	require.EqualValues(t, 1, dirty)
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.Equal(t, epochBefore+3, epoch)
}

func TestCustodiansForVersionOrdersDocumentBeforeCollectionClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "Synthetic custodian collection")
	require.NoError(t, err)
	node, added, err := s.IngestFile(ctx, run, s.RootID(), "record.txt", fakeHash("c1"),
		4, "text/plain", "/synthetic/record.txt", "")
	require.NoError(t, err)
	require.True(t, added)
	collection, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "collection", IngestID: run.ID()},
		RawLabel: "Collection owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)
	transfer, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID},
		RawLabel: "Transferred owner", Rank: "additional", Basis: "transfer_record", IfMatchRevision: 1})
	require.NoError(t, err)
	operator, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID},
		RawLabel: "Operator owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)

	rows, err := s.CustodiansForVersion(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []string{operator.AssignmentID, transfer.AssignmentID, collection.AssignmentID}, []string{
		rows[0].AssignmentID, rows[1].AssignmentID, rows[2].AssignmentID,
	})
	require.Equal(t, []int{0, 1, 4}, []int{custodianPrecedence(rows[0]), custodianPrecedence(rows[1]), custodianPrecedence(rows[2])})
}

func TestCustodiansDistinguishesPackageDefaultFromAllRecords(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "package", PackageID: "package-a"},
		RawLabel: "Default owner", Rank: "additional", Basis: "package_column", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "package", PackageID: "package-a", PackageRecordID: "record-1"},
		RawLabel: "Record owner", Rank: "additional", Basis: "package_column", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "package", PackageID: "package-b"},
		PersonID: mustCreateCustodianPerson(t, s, "Resolved owner"), RawLabel: "Resolved owner", Rank: "additional", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)

	allPackageRows, total, err := s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: "package-a"}, false, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, allPackageRows, 2)
	defaultRows, total, err := s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: "package-a", HasPackageRecordID: true}, false, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, defaultRows, 1)
	require.Nil(t, defaultRows[0].IngestID)
	require.Nil(t, defaultRows[0].NodeID)
	require.Nil(t, defaultRows[0].ContentVersionID)
	require.NotNil(t, defaultRows[0].PackageID)
	require.NotNil(t, defaultRows[0].PackageRecordID)
	require.Empty(t, *defaultRows[0].PackageRecordID)
	recordRows, total, err := s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: "package-a", PackageRecordID: "record-1", HasPackageRecordID: true}, false, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "Record owner", recordRows[0].RawLabel)
	_, _, err = s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: "package-a", PackageRecordID: "record-1"}, false, 100, 0)
	require.ErrorIs(t, err, ErrInvalidPerson)
	unresolved, total, err := s.Custodians(ctx, CustodianScope{}, true, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, unresolved, 2)
}

func TestCustodianAuthorityRejectsInvalidValuesAndReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, version := seedPeopleVersion(t, s)
	scope := CustodianScope{Kind: "document", NodeID: node, ContentVersionID: version}
	request := CustodianRequest{Scope: scope, RawLabel: "Owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1}

	invalid := request
	invalid.RawLabel = strings.Repeat("x", 201)
	_, err := s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrInvalidPerson)
	invalid = request
	invalid.SourceRef = strings.Repeat("x", 513)
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrInvalidPerson)
	invalid = request
	invalid.Rank = "owner"
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrInvalidPerson)
	invalid = request
	invalid.Basis = "filesystem_owner"
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrInvalidPerson)
	invalid = request
	invalid.Scope.NodeID++
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrNotFound)
	invalid = request
	invalid.PersonID = "00000000-0000-4000-8000-000000000000"
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrNotFound)
	person, err := s.CreatePerson(ctx, "Retired owner", "operator")
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	invalid = request
	invalid.PersonID = person.PersonID
	_, err = s.SetCustodian(ctx, invalid)
	require.ErrorIs(t, err, ErrPersonRetired)
	_, _, err = s.Custodians(ctx, CustodianScope{}, false, 251, 0)
	require.ErrorIs(t, err, ErrInvalidPerson)
}

func mustCreateCustodianPerson(t *testing.T, s *Store, name string) string {
	t.Helper()
	person, err := s.CreatePerson(t.Context(), name, "operator")
	require.NoError(t, err)
	return person.PersonID
}
