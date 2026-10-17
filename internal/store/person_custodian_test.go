package store

import (
	"fmt"
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
	var hiddenOnlyID string
	var hiddenFallbackIDs, visibleIDs []string
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
			if version == hidden.CurrentVersionID {
				if action == "assert" {
					hiddenOnlyID = p.PersonID
				} else {
					hiddenFallbackIDs = append(hiddenFallbackIDs, p.PersonID)
				}
			} else {
				visibleIDs = append(visibleIDs, p.PersonID)
			}
		}
	}
	afterName, afterID := "", ""
	for _, id := range append(hiddenFallbackIDs, visibleIDs...) {
		page, err := s.PeopleByDisplayName(ctx, "Alex", afterName, afterID, 1, true)
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.Equal(t, id, page[0].PersonID)
		afterName, afterID = page[0].DisplayNameFolded, page[0].PersonID
	}
	last, err := s.PeopleByDisplayName(ctx, "Alex", afterName, afterID, 1, true)
	require.NoError(t, err)
	require.Empty(t, last)
	withHiddenPhoto, err := s.PeopleByDisplayName(unlocked, "Alex", "", "", 1, true)
	require.NoError(t, err)
	require.Len(t, withHiddenPhoto, 1)
	require.Equal(t, hiddenOnlyID, withHiddenPhoto[0].PersonID)
}

func TestPeopleByDisplayNameRequiresPublishedEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("search-hidden"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	candidate, err := s.CreatePerson(ctx, "Alex Candidate", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(unlocked, hidden.CurrentVersionID, "11111111-1111-4111-8111-111111111111", document.FaceBox{Width: .2, Height: .2}, "")

	require.NoError(t, err)
	buildFaceMatchCandidateForTest(t, s, hidden.CurrentVersionID, face.FaceID, 99, 0)
	_, err = s.db.ExecContext(ctx, `UPDATE person_match_candidates SET suggested_person_id=? WHERE actor_key=?`, candidate.PersonID, "face:"+face.FaceID)
	require.NoError(t, err)
	people, err := s.PeopleByDisplayName(ctx, "Alex Candidate", "", "", 10, true)
	require.NoError(t, err)
	require.Len(t, people, 1)
	retainedHidden, err := s.CreatePerson(ctx, "Hidden Ada", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(unlocked, hidden.CurrentVersionID, retainedHidden.PersonID)
	require.NoError(t, err)
	retainedHidden, _, err = s.PersonByID(ctx, retainedHidden.PersonID)
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, retainedHidden.PersonID, retainedHidden.Revision, PersonIdentity{
		Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "visible-derived-author", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	derivedVersion := seedDocumentPeopleEvent(t, s, "visible-person-author.txt", "d9", nil)
	derived := publishLifecycleDocumentPeople(t, s, derivedVersion.ID)
	require.Equal(t, retainedHidden.PersonID, derived.PersonID)
	locked, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 100, true)
	require.NoError(t, err)
	personIDs := func(people []PersonSearchResult) []string {
		ids := make([]string, 0, len(people))
		for _, person := range people {
			ids = append(ids, person.PersonID)
		}
		return ids
	}
	require.Equal(t, []string{retainedHidden.PersonID}, personIDs(locked))
	all, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 100, false)
	require.NoError(t, err)
	require.Equal(t, []string{retainedHidden.PersonID}, personIDs(all))
	withAccess, err := s.PeopleByDisplayName(unlocked, "Hidden Ada", "", "", 100, true)
	require.NoError(t, err)
	require.Equal(t, personIDs(locked), personIDs(withAccess))
	for _, action := range []string{"suppress", "withdrawn"} {
		_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET action=?,photo_mark=0 WHERE person_id=?`, action, retainedHidden.PersonID)
		require.NoError(t, err)
		visibleDerived, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 10, true)
		require.NoError(t, err)
		require.Len(t, visibleDerived, 1)
		require.Equal(t, retainedHidden.PersonID, visibleDerived[0].PersonID)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_heads SET node_revision=node_revision+1 WHERE content_version_id=?`, derivedVersion.ID)
	require.NoError(t, err)
	stale, err := s.PeopleByDisplayName(ctx, "Hidden Ada", "", "", 10, true)
	require.NoError(t, err)
	require.Len(t, stale, 1)
	require.Equal(t, retainedHidden.PersonID, stale[0].PersonID)
	for _, state := range []string{"stale", "trashed"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			version := seedDocumentPeopleEvent(t, s, "retained-person.txt", "d9", nil)
			person := publishLifecycleDocumentPeople(t, s, version.ID)
			if state == "stale" {
				_, _, err := s.ReplaceContent(ctx, version.NodeID, UnconditionalRev, fakeHash("e9"), 1, "text/plain")
				require.NoError(t, err)
			} else {
				_, _, err := s.Trash(ctx, version.NodeID, UnconditionalRev)
				require.NoError(t, err)
			}
			var retained int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM document_people WHERE person_id=?`, person.PersonID).Scan(&retained))
			require.Positive(t, retained)
			all, err := s.PeopleByDisplayName(ctx, person.DisplayName, "", "", 10, false)
			require.NoError(t, err)
			require.Len(t, all, 1)
			retainedVisible, err := s.PeopleByDisplayName(ctx, person.DisplayName, "", "", 10, true)
			require.NoError(t, err)
			require.Equal(t, all, retainedVisible)
		})
	}
	var sensitiveID, visibleID string
	for _, sensitive := range []bool{true, false} {
		address, name, seed := "visible@example.test", "Published Visible", "d1"
		if sensitive {
			address, name, seed = "sensitive@example.test", "Published Sensitive", "d2"
		}
		version := seedDocumentPeopleEvent(t, s, seed+".txt", seed, []document.DocumentEventActorV1{{
			ActorKey: "email:" + address, Address: address, DisplayName: name,
			Claim: name, Role: "author", Sensitive: sensitive,
		}})
		input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
		require.NoError(t, err)
		require.Len(t, input.Actors, 1)
		require.Equal(t, sensitive, input.Actors[0].Sensitive)
		publication := documentPeoplePublicationForInput(t, input)
		publication.People.Edges[0].Sensitive = input.Actors[0].Sensitive
		_, err = s.PublishDocumentPeople(ctx, publication)
		require.NoError(t, err)
		if sensitive {
			sensitiveID = publication.People.Edges[0].PersonID
		} else {
			visibleID = publication.People.Edges[0].PersonID
		}
	}
	for _, unlocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("published/unlocked=%t", unlocked), func(t *testing.T) {
			searchCtx := ctx
			if unlocked {
				searchCtx = WithPhotoHiddenToken(ctx, token)
			}
			global, err := s.PeopleByDisplayName(searchCtx, "Published", "", "", 10, false)
			require.NoError(t, err)
			require.Len(t, global, 2)
			require.Equal(t, sensitiveID, global[0].PersonID)
			require.Equal(t, "Published Sensitive", global[0].DisplayName)
			require.Equal(t, visibleID, global[1].PersonID)
			browser, err := s.PeopleByDisplayName(searchCtx, "Published", "", "", 10, true)
			require.NoError(t, err)
			require.Len(t, browser, 1)
			require.Equal(t, visibleID, browser[0].PersonID)
		})
	}
	version := seedDocumentPeopleEvent(t, s, "pending.txt", "b1", []document.DocumentEventActorV1{{ActorKey: "email:pending@example.test", Address: "pending@example.test", DisplayName: "Pending Person", Claim: "Pending Person", Role: "author", Sensitive: true}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	global, err := s.PeopleByDisplayName(ctx, "Pending", "", "", 10, false)
	require.NoError(t, err)
	require.Len(t, global, 1)
	for _, stage := range []string{"pending", "failed"} {
		if stage == "failed" {
			require.NoError(t, s.MarkDocumentPeopleFailed(ctx, input, "derivation_failed"))
		}
		t.Run(stage, func(t *testing.T) {
			browser, err := s.PeopleByDisplayName(ctx, "Pending", "", "", 10, true)
			require.NoError(t, err)
			require.Empty(t, browser, "derived people must wait for visible publication")
		})
	}
}

func TestPeopleByDisplayNameSensitiveCandidateAcceptance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "candidate.txt", "b2", []document.DocumentEventActorV1{{ActorKey: "name_alias:alex sensitive", DisplayName: "Alex Sensitive", Claim: "Alex Sensitive", Role: "author", Sensitive: true}})
	_, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	candidates, _, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidates[0].CandidateID, Action: "new_person", ExpectedRevision: candidates[0].Revision})
	require.NoError(t, err)
	assertions, err := s.DocumentPersonAssertions(ctx, version.ID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, "author", assertions[0].Role)
	browser, err := s.PeopleByDisplayName(ctx, "Alex Sensitive", "", "", 10, true)
	require.NoError(t, err)
	require.Empty(t, browser)
	visible := browsePhotoNode(t, s, "visible.jpg", fakeHash("candidate-visible"), "image/jpeg")
	_, err = s.MarkPersonInPhoto(ctx, visible.CurrentVersionID, decided.DecidedPersonID)
	require.NoError(t, err)
	browser, err = s.PeopleByDisplayName(ctx, "Alex Sensitive", "", "", 10, true)
	require.NoError(t, err)
	require.Len(t, browser, 1)
	require.Equal(t, decided.DecidedPersonID, browser[0].PersonID)
}

func TestPeopleByDisplayNameUnicodeWordPrefixes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"John,Smith", "John—Smith", "John/Smith", "John_Smith", "John(Smith)", "John,Smith%", "John,Smith_", "李·明", "李明", "X\u0301Jane", "2Jane"} {
		_, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"SMITH", []string{"John(Smith)", "John,Smith", "John,Smith%", "John,Smith_", "John/Smith", "John_Smith", "John—Smith"}},
		{"Smith%", []string{"John,Smith%"}},
		{"Smith_", []string{"John,Smith_"}},
		{"明", []string{"李·明"}},
		{"Jane", nil},
	} {
		var names []string
		afterName, afterID := "", ""
		for {
			page, err := s.PeopleByDisplayName(ctx, tc.query, afterName, afterID, 2, true)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			for _, person := range page {
				names = append(names, person.DisplayName)
			}
			last := page[len(page)-1]
			afterName, afterID = last.DisplayNameFolded, last.PersonID
		}
		require.Equal(t, tc.want, names, "query %q", tc.query)
	}
}

func TestPeopleByDisplayNameMatchesWordPrefixes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"John Smith", "Smith Jones", "Alice Smith", "Alice Blacksmith", "Alice Smith%", "Grace Hopper", "GRACE Murray Hopper", "Straße", "STRASSE", "Mary-Jane", "O'Brien", "Mary\tJane", "Mary\nJane", "Alice Mary-Jane", "John O'Brien", "O’Brien"} {
		_, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
	}
	for query, count := range map[string]int{"Jane": 4, "Brien": 3, "Mary-J": 2, "O'Brien": 2} {
		matches, err := s.PeopleByDisplayName(ctx, query, "", "", 10, true)
		require.NoError(t, err)
		require.Len(t, matches, count)
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
	for _, query := range []string{"grace", "strasse"} {
		first, err := s.PeopleByDisplayName(ctx, query, "", "", 1, false)
		require.NoError(t, err)
		require.Len(t, first, 1)
		second, err := s.PeopleByDisplayName(ctx, query, first[0].DisplayNameFolded, first[0].PersonID, 10, false)
		require.NoError(t, err)
		require.Len(t, second, 1)
		if query == "grace" {
			require.Equal(t, "Grace Hopper", first[0].DisplayName)
			require.Equal(t, "GRACE Murray Hopper", second[0].DisplayName)
		} else {
			require.Equal(t, "strasse", first[0].DisplayNameFolded)
			require.Equal(t, "strasse", second[0].DisplayNameFolded)
			require.ElementsMatch(t, []string{"Straße", "STRASSE"}, []string{first[0].DisplayName, second[0].DisplayName})
		}
	}
}

func TestPeopleSearchIdentityLabelsRespectEvidenceVisibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	visible := browsePhotoNode(t, s, "identity-visible.jpg", fakeHash("identity-visible"), "image/jpeg")
	hidden := browsePhotoNode(t, s, "identity-hidden.jpg", fakeHash("identity-hidden"), "image/jpeg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	for _, test := range []struct {
		name, kind, value, origin, scopeKind, scopeValue, expected string
		sensitive, hidden                                          bool
	}{
		{name: "operator", kind: "email", value: "operator@example.test", origin: "operator", expected: "operator@example.test"},
		{name: "visible", kind: "email", value: "visible@example.test", origin: "derived", expected: "visible@example.test"},
		{name: "unsupported", kind: "email", value: "unsupported@example.test", origin: "derived"},
		{name: "sensitive", kind: "email", value: "sensitive@example.test", origin: "derived", sensitive: true},
		{name: "hidden", kind: "email", value: "hidden@example.test", origin: "derived", hidden: true},
		{name: "phone", kind: "phone", value: "+12025550123", origin: "operator", expected: "+12025550123"},
		{name: "handle", kind: "handle", value: "chat/user-a", origin: "operator", scopeKind: "workspace", scopeValue: "example", expected: "chat/user-a (workspace: example)"},
		{name: "unscoped", kind: "handle", value: "chat/user-a", origin: "operator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			person, err := s.CreatePerson(ctx, "Alex "+test.name, "operator")
			require.NoError(t, err)
			_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: visible.CurrentVersionID, PersonID: person.PersonID, Role: "depicted", Action: "assert", Revision: 1})
			require.NoError(t, err)
			person, _, err = s.PersonByID(ctx, person.PersonID)
			require.NoError(t, err)
			_, err = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{
				Kind: test.kind, ValueDisplay: test.value, Origin: test.origin, ScopeKind: test.scopeKind, ScopeValue: test.scopeValue,
				EvidenceKind: "source_metadata", EvidenceID: "identity-source", Confidence: "exact_identifier",
			})
			require.NoError(t, err)
			source := visible
			if test.hidden {
				source = hidden
			}
			if test.name != "unsupported" {
				_, err = s.db.ExecContext(ctx, `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive) VALUES(?,?,?,'synthetic','author',?,'source_metadata','identity-source','exact_identifier','identifier_match','Alex',1,?)`, source.CurrentVersionID, source.ID, person.PersonID, test.kind+":"+test.value, test.sensitive)
				require.NoError(t, err)
			}
			if test.sensitive {
				_, err = s.db.ExecContext(ctx, `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive) VALUES(?,?,?,'synthetic','author','email:other@example.test','source_metadata','identity-source','exact_identifier','identifier_match','Alex',1,0)`, visible.CurrentVersionID, visible.ID, person.PersonID)
				require.NoError(t, err)
			}
			page, err := s.PeopleByDisplayName(ctx, person.DisplayName, "", "", 10, true)
			require.NoError(t, err)
			require.Len(t, page, 1)
			require.Equal(t, test.expected, page[0].IdentityLabel)
			if test.hidden {
				page, err = s.PeopleByDisplayName(WithPhotoHiddenToken(ctx, token), person.DisplayName, "", "", 10, true)
				require.NoError(t, err)
				require.Equal(t, test.value, page[0].IdentityLabel)
			}
		})
	}
}

func TestPeopleSearchPrefersEmailThenNormalizedValue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Alex Smith", "operator")
	require.NoError(t, err)
	for _, value := range []struct{ kind, display string }{{"phone", "+12025550123"}, {"email", "z@example.test"}, {"email", "a@example.test"}} {
		_, err = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{Kind: value.kind, ValueDisplay: value.display, Origin: "operator", EvidenceKind: "operator_assertion", EvidenceID: "label-order", Confidence: "operator_asserted"})
		require.NoError(t, err)
		person.Revision++
	}
	page, err := s.PeopleByDisplayName(ctx, "Alex", "", "", 10, true)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "a@example.test", page[0].IdentityLabel)
}
