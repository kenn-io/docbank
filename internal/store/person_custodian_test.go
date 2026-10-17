package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestCustodianScopeRejectsMixedCoordinates(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "collection", IngestID: "ingest-a", ContentVersionID: "version-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a"}))
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a", NodeID: 2}))
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "package", PackageID: "package-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "package", PackageID: "package-a", PackageRecordID: "record-a"}))
	require.NoError(t, validateCustodianScope(CustodianScope{Kind: "package", PackageID: "package-a", PackageRecordID: "record-a", HasPackageRecordID: true}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "package", PackageRecordID: "record-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "package", PackageID: "package-a", IngestID: "ingest-a"}))
	require.Error(t, validateCustodianScope(CustodianScope{Kind: "document", ContentVersionID: "version-a", NodeID: 2, PackageID: "package-a"}))
}

func TestCustodianPackageScopeKeepsDefaultAndRecordsSeparate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pkg, node := seedReceivedPackage(t, s, "custodian-package")
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "CUS000001")
	otherRequest := pkg.PackageRequest
	otherID, err := newUUIDv4()
	require.NoError(t, err)
	otherRequest.PackageID = otherID
	otherRequest.PackageName = "other-custodian-package"
	run, err := s.PackageIngestRun(ctx, pkg.PackageID)
	require.NoError(t, err)
	_, err = s.AdmitPackageImport(ctx, run, otherRequest, packageImportJobRequest(t, s, otherRequest))
	require.NoError(t, err)
	otherPackage, err := s.Package(ctx, otherID)
	require.NoError(t, err)
	commitReceivedLabel(t, s, otherPackage, node.CurrentVersionID, "OTH000001")
	packageID := pkg.PackageID
	recordID, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	defaultScope := CustodianScope{Kind: "package", PackageID: packageID}
	recordScope := CustodianScope{Kind: "package", PackageID: packageID, PackageRecordID: recordID, HasPackageRecordID: true}
	otherScope := CustodianScope{Kind: "package", PackageID: otherPackage.PackageID, PackageRecordID: recordID, HasPackageRecordID: true}
	set := func(scope CustodianScope, label string) CustodianAssignment {
		t.Helper()
		assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: label,
			Rank: "primary", Basis: "package_column", SourceRef: "Custodian", IfMatchRevision: 1})
		require.NoError(t, err)
		return assignment
	}
	def := set(defaultScope, "Default owner")
	record := set(recordScope, "Record owner")
	other := set(otherScope, "Other sender")
	require.Nil(t, record.IngestID)
	require.Nil(t, record.NodeID)
	require.Nil(t, record.ContentVersionID)
	require.Equal(t, packageID, *record.PackageID)
	require.Equal(t, recordID, *record.PackageRecordID)
	require.Empty(t, *def.PackageRecordID)

	all, total, err := s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: packageID}, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, all, 2)
	defaults, total, err := s.Custodians(ctx, CustodianScope{Kind: "package", PackageID: packageID, HasPackageRecordID: true}, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, def.AssignmentID, defaults[0].AssignmentID)
	records, total, err := s.Custodians(ctx, recordScope, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, record.AssignmentID, records[0].AssignmentID)
	others, total, err := s.Custodians(ctx, otherScope, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, other.AssignmentID, others[0].AssignmentID)
}

func TestCustodianAssignmentUsesRealVersion(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestPeopleByDisplayNameUsesFoldedStableCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	grace, err := s.CreatePerson(ctx, "Grace Hopper", "operator")
	require.NoError(t, err)
	ada, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	secondGrace, err := s.CreatePerson(ctx, "GRACE Murray Hopper", "operator")
	require.NoError(t, err)

	page, err := s.PeopleByDisplayName(ctx, "grace", "", "", 1, false)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, grace.PersonID, page[0].PersonID)
	next, err := s.PeopleByDisplayName(ctx, "grace", page[0].DisplayNameFolded, page[0].PersonID, 10, false)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, secondGrace.PersonID, next[0].PersonID)

	hiddenNode := browsePhotoNode(t, s, "hidden-person.jpg", fakeHash("hidden-person"), "image/jpeg")
	visibleNode := browsePhotoNode(t, s, "visible-person.jpg", fakeHash("visible-person"), "image/jpeg")
	hidden, err := s.CreatePerson(ctx, "Hidden Ada", "operator")
	require.NoError(t, err)
	visible, err := s.CreatePerson(ctx, "Visible Lin", "operator")
	require.NoError(t, err)
	free, err := s.CreatePerson(ctx, "Free Grace", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, hiddenNode.CurrentVersionID, hidden.PersonID)
	require.NoError(t, err)
	hidden, _, err = s.PersonByID(ctx, hidden.PersonID)
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, hidden.PersonID, hidden.Revision, PersonIdentity{
		Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "visible-derived-author", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	derivedVersion := seedDocumentPeopleEvent(t, s, "visible-person-author.txt", "d9", nil)
	derived := publishLifecycleDocumentPeople(t, s, derivedVersion.ID)
	require.Equal(t, hidden.PersonID, derived.PersonID)
	_, err = s.MarkPersonInPhoto(ctx, visibleNode.CurrentVersionID, visible.PersonID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, hiddenNode.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	locked, err := s.PeopleByDisplayName(ctx, "", "", "", 100, true)
	require.NoError(t, err)
	personIDs := func(people []Person) []string {
		ids := make([]string, 0, len(people))
		for _, person := range people {
			ids = append(ids, person.PersonID)
		}
		return ids
	}
	require.Equal(t, []string{ada.PersonID, free.PersonID, grace.PersonID, secondGrace.PersonID, hidden.PersonID, visible.PersonID}, personIDs(locked))
	all, err := s.PeopleByDisplayName(ctx, "", "", "", 100, false)
	require.NoError(t, err)
	require.Equal(t, []string{ada.PersonID, free.PersonID, grace.PersonID, secondGrace.PersonID, hidden.PersonID, visible.PersonID}, personIDs(all))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked, err := s.PeopleByDisplayName(WithPhotoHiddenToken(ctx, token), "", "", "", 100, true)
	require.NoError(t, err)
	require.Equal(t, personIDs(all), personIDs(unlocked))
	for _, action := range []string{"suppress", "withdrawn"} {
		_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET action=?,photo_mark=0 WHERE person_id=?`, action, hidden.PersonID)
		require.NoError(t, err)
		visibleDerived, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 10, true)
		require.NoError(t, err)
		require.Len(t, visibleDerived, 1)
		require.Equal(t, hidden.PersonID, visibleDerived[0].PersonID)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_heads SET node_revision=node_revision+1 WHERE content_version_id=?`, derivedVersion.ID)
	require.NoError(t, err)
	stale, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 10, true)
	require.NoError(t, err)
	require.Empty(t, stale)
}

func TestResolveCustodianPreservesSenderClaimAndResolvesAlias(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pkg, node := seedReceivedPackage(t, s, "resolve-custodian")
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "CUS000010")
	recordID, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "package", PackageID: pkg.PackageID,
		PackageRecordID: recordID, HasPackageRecordID: true}, RawLabel: "Sender Label", Rank: "primary",
		Basis: "package_column", SourceRef: "CUSTODIAN", IfMatchRevision: 1})
	require.NoError(t, err)
	survivor, err := s.CreatePerson(ctx, "Canonical Person", "operator")
	require.NoError(t, err)
	alias, err := s.CreatePerson(ctx, "Old Person", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, alias.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, alias.Revision)
	require.NoError(t, err)

	resolved, err := s.ResolveCustodian(ctx, assignment.AssignmentID, alias.PersonID, assignment.Revision)
	require.NoError(t, err)
	require.Equal(t, assignment.AssignmentID, resolved.AssignmentID)
	require.Equal(t, assignment.RawLabel, resolved.RawLabel)
	require.Equal(t, assignment.Rank, resolved.Rank)
	require.Equal(t, assignment.Basis, resolved.Basis)
	require.Equal(t, assignment.SourceRef, resolved.SourceRef)
	require.Equal(t, survivor.PersonID, *resolved.PersonID)
	require.EqualValues(t, 2, resolved.Revision)
	_, err = s.ResolveCustodian(ctx, assignment.AssignmentID, survivor.PersonID, 1)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestOperatorPackageCustodianDoesNotReplaceSenderPrimary(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pkg, node := seedReceivedPackage(t, s, "operator-custodian")
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "CUS000020")
	scope := CustodianScope{Kind: "package", PackageID: pkg.PackageID}
	_, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Sender Owner", Rank: "primary",
		Basis: "package_column", SourceRef: "CUSTODIAN", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = s.SetOperatorPackageCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Operator Owner",
		Rank: "primary", Basis: "operator_assigned", SourceRef: "operator", IfMatchRevision: 1})
	require.ErrorIs(t, err, ErrCustodianConflict)
}

func TestPeopleByDisplayNameFiltersAllHiddenEvidenceBeforePaging(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("search-hidden"), "image/jpeg")
	visible := browsePhotoNode(t, s, "visible.jpg", fakeHash("search-visible"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	for _, action := range []string{"assert", "suppress", "withdrawn"} {
		for _, version := range []string{hidden.CurrentVersionID, visible.CurrentVersionID} {
			prefix := "Alex Z"
			if version == hidden.CurrentVersionID {
				prefix = "Alex A"
			}
			p, err := s.CreatePerson(ctx, prefix+action+version, "operator")
			require.NoError(t, err)
			_, err = s.AssertDocumentPerson(unlocked, PersonDocumentAssertion{ContentVersionID: version, PersonID: p.PersonID, Role: "depicted", Action: action, Revision: 1})
			require.NoError(t, err)
			people, err := s.PeopleByDisplayName(ctx, p.DisplayName, "", "", 10, true)
			require.NoError(t, err)
			if version == hidden.CurrentVersionID {
				require.Empty(t, people)
			} else {
				require.Len(t, people, 1)
				require.Equal(t, p.PersonID, people[0].PersonID)
			}
		}
	}
	candidate, err := s.CreatePerson(ctx, "Alex Candidate", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(unlocked, hidden.CurrentVersionID, "11111111-1111-4111-8111-111111111111", document.FaceBox{Width: .2, Height: .2}, "")

	require.NoError(t, err)
	buildFaceMatchCandidateForTest(t, s, hidden.CurrentVersionID, face.FaceID, 99, 0)
	_, err = s.db.ExecContext(ctx, `UPDATE person_match_candidates SET suggested_person_id=? WHERE actor_key=?`, candidate.PersonID, "face:"+face.FaceID)
	require.NoError(t, err)
	people, err := s.PeopleByDisplayName(ctx, "Alex Candidate", "", "", 10, true)
	require.NoError(t, err)
	require.Empty(t, people)
	free, err := s.CreatePerson(ctx, "Alex Free", "operator")
	require.NoError(t, err)
	page, err := s.PeopleByDisplayName(ctx, "Alex", "", "", 1, true)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, free.PersonID, page[0].PersonID)
	next, err := s.PeopleByDisplayName(ctx, "Alex", page[0].DisplayNameFolded, page[0].PersonID, 10, true)
	require.NoError(t, err)
	require.Len(t, next, 3)
	for _, p := range next {
		require.NotEqual(t, candidate.PersonID, p.PersonID)
	}
}

func TestPeopleByDisplayNameMatchesWordPrefixes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"John Smith", "Smith Jones", "Alice Smith", "Alice Blacksmith", "Alice Smith%"} {
		_, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
	}
	page, err := s.PeopleByDisplayName(ctx, "Smith", "", "", 1, false)
	require.NoError(t, err)
	require.Equal(t, "Alice Smith", page[0].DisplayName)
	next, err := s.PeopleByDisplayName(ctx, "Smith", page[0].DisplayNameFolded, page[0].PersonID, 10, false)
	require.NoError(t, err)
	require.Equal(t, []string{"Alice Smith%", "John Smith", "Smith Jones"}, []string{next[0].DisplayName, next[1].DisplayName, next[2].DisplayName})
	literal, err := s.PeopleByDisplayName(ctx, "Smith%", "", "", 10, false)
	require.NoError(t, err)
	require.Len(t, literal, 1)
	require.Equal(t, "Alice Smith%", literal[0].DisplayName)
}

func BenchmarkPeopleByDisplayNameHiddenCatalog(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "people.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", fakeHash("benchmark-hidden"), 12, "image/jpeg")
	require.NoError(b, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(b, err)
	require.NoError(b, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(b, err)
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE seq(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n<20000) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) SELECT printf('00000000-0000-4000-8000-%012d',n),printf('Alex %05d',n),printf('alex %05d',n),'operator','curated',?,? FROM seq`, nowRFC3339(), nowRFC3339())
	require.NoError(b, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at) SELECT person_id,?,person_id,'depicted','suppress','',? FROM persons WHERE display_name_folded<'alex 10001'`, node.CurrentVersionID, nowRFC3339())
	require.NoError(b, err)
	for b.Loop() {
		people, err := s.PeopleByDisplayName(ctx, "Alex", "", "", 100, true)
		require.NoError(b, err)
		require.Len(b, people, 100)
		require.Equal(b, "Alex 10001", people[0].DisplayName)
	}
}
