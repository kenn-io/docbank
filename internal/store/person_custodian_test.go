package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustodianScopeRejectsMixedCoordinates(t *testing.T) {
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a", ContentVersionID: "version-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a"}))
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a", NodeID: 2}))
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

func TestCustodianAdditionalRejectsDuplicateClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, version := seedPeopleVersion(t, s)
	request := CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: node, ContentVersionID: version},
		RawLabel: "Records Team", Rank: "additional", Basis: "transfer_record", SourceRef: "receipt-a", IfMatchRevision: 1}
	_, err := s.SetCustodian(ctx, request)
	require.NoError(t, err)
	request.RawLabel = "RECORDS TEAM"
	_, err = s.SetCustodian(ctx, request)
	require.ErrorIs(t, err, ErrCustodianConflict)
	rows, err := s.CustodiansForVersion(ctx, version)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	request.SourceRef = "receipt-b"
	_, err = s.SetCustodian(ctx, request)
	require.NoError(t, err)
	rows, err = s.CustodiansForVersion(ctx, version)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestCustodianCollectionAppliesOnlyToMembers(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, unrelatedVersion := seedPeopleVersion(t, s)
	run, err := s.BeginIngest(ctx, "cli", "Synthetic custodian collection")
	require.NoError(t, err)
	var versions []string
	for i := range 2 {
		name := fmt.Sprintf("record-%d.txt", i)
		node, _, err := s.IngestFile(ctx, run, s.RootID(), name, fakeHash(fmt.Sprintf("c%d", i)),
			4, "text/plain", "/synthetic/"+name, "")
		require.NoError(t, err)
		versions = append(versions, node.CurrentVersionID)
	}
	request := CustodianRequest{Scope: CustodianScope{Kind: "collection", IngestID: run.ID()},
		RawLabel: "Collection owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1}
	assignment, err := s.SetCustodian(ctx, request)
	require.NoError(t, err)
	for _, version := range versions {
		rows, err := s.CustodiansForVersion(ctx, version)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, assignment.AssignmentID, rows[0].AssignmentID)
	}
	rows, err := s.CustodiansForVersion(ctx, unrelatedVersion)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestCustodianCollectionRejectsCallerSuppliedProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginCallerSuppliedIngest(ctx, "cli", "Synthetic source assertion")
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, run, s.RootID(), "record.txt", fakeHash("c1"),
		4, "text/plain", "/synthetic/record.txt", "")
	require.NoError(t, err)
	_, err = s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "collection", IngestID: run.ID()},
		RawLabel: "Collection owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCustodiansIncludesRetiredPersonAsUnresolved(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, version := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(ctx, "Records owner", "operator")
	require.NoError(t, err)
	assignment, err := s.SetCustodian(ctx, CustodianRequest{
		Scope: CustodianScope{Kind: "document", NodeID: node, ContentVersionID: version}, PersonID: person.PersonID,
		RawLabel: "Records owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	unresolved, total, err := s.Custodians(ctx, CustodianScope{}, true, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, unresolved, 1)
	require.Equal(t, assignment.AssignmentID, unresolved[0].AssignmentID)
	require.Equal(t, assignment.RawLabel, unresolved[0].RawLabel)
	require.Nil(t, unresolved[0].PersonID)
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
	var epoch int64
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
