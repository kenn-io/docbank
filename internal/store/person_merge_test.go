package store

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSplitRequiresExplicitMembership(t *testing.T) {
	t.Parallel()
	request := PersonSplitRequest{PersonID: "00000000-0000-4000-8000-000000000001",
		OperationID: "00000000-0000-4000-8000-000000000002", Revision: 1}
	require.ErrorIs(t, validatePersonSplitRequest(request), ErrInvalidPerson)
	request.IdentityIDs = []string{"00000000-0000-4000-8000-000000000003"}
	require.NoError(t, validatePersonSplitRequest(request))
	request.IdentityIDs = append(request.IdentityIDs, request.IdentityIDs[0])
	require.Error(t, validatePersonSplitRequest(request))
}

func addTestPersonIdentity(t *testing.T, s *Store, person Person, kind, value, evidenceID string) (Person, PersonIdentity) {
	t.Helper()
	identity, err := s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: kind, ValueDisplay: value, Origin: "operator", EvidenceKind: "operator_assertion",
		EvidenceID: evidenceID, Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	return person, identity
}

func TestMergePersonsDeduplicatesAliasesAndReplaysReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "A. Lovelace", "operator")
	require.NoError(t, err)
	survivor, _ = addTestPersonIdentity(t, s, survivor, "name_alias", "Ada", "survivor-alias")
	absorbed, absorbedAlias := addTestPersonIdentity(t, s, absorbed, "name_alias", "ADA", "absorbed-alias")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	receipt, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Contains(t, receipt.Moved.IdentityIDs, absorbedAlias.IdentityID)
	require.Equal(t, []PersonMergeDeduplicatedIdentity{{
		IdentityID: absorbedAlias.IdentityID, Kind: "name_alias", ValueNormalized: "ada", ValueDisplay: "ADA",
		Normalization: "casefold", Origin: "operator", EvidenceKind: "operator_assertion",
		EvidenceID: "absorbed-alias", Confidence: "operator_asserted", RecordedAt: absorbedAlias.RecordedAt,
		RetainedIdentityID: survivorAliasID(t, s, survivor.PersonID),
	}}, receipt.Moved.DeduplicatedIdentities)
	resolved, through, err := s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	require.Equal(t, survivor.PersonID, resolved.PersonID)
	require.Equal(t, absorbed.PersonID, through)
	replayed, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision+1, absorbed.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergePersonsPreservesDuplicateAssertionTimeAndRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Absorbed", "operator")
	require.NoError(t, err)
	third, err := s.CreatePerson(ctx, "Third", "operator")
	require.NoError(t, err)
	duplicate := browsePhotoNode(t, s, "duplicate.jpg", fakeHash("duplicate-evidence"), "image/jpeg")
	newer := browsePhotoNode(t, s, "newer.jpg", fakeHash("newer-evidence"), "image/jpeg")
	keptAssertion, err := s.MarkPersonInPhoto(ctx, duplicate.CurrentVersionID, survivor.PersonID, "")
	require.NoError(t, err)
	absorbedAssertion, err := s.MarkPersonInPhoto(ctx, duplicate.CurrentVersionID, absorbed.PersonID, "")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, newer.CurrentVersionID, third.PersonID, "")
	require.NoError(t, err)
	for assertionID, recordedAt := range map[string]string{
		keptAssertion.AssertionID:     "2026-01-01T00:00:00Z",
		absorbedAssertion.AssertionID: "2026-03-01T00:00:00Z",
	} {
		_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at=? WHERE assertion_id=?`, recordedAt, assertionID)
		require.NoError(t, err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at='2026-02-01T00:00:00Z' WHERE person_id=?`, third.PersonID)
	require.NoError(t, err)
	before, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(t, err)
	require.Equal(t, absorbed.PersonID, before[0].PersonID)
	require.Equal(t, third.PersonID, before[1].PersonID)

	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	merged, err := s.DocumentPersonAssertions(ctx, duplicate.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, merged, 1)
	require.Equal(t, "2026-01-01T00:00:00Z", merged[0].RecordedAt)
	after, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(t, err)
	require.Equal(t, third.PersonID, after[0].PersonID)
	require.Equal(t, survivor.PersonID, after[1].PersonID)
	require.Equal(t, "2026-01-01T00:00:00Z", after[1].RecentRank)
}

func TestMergePersonsRejectsMovedRecordsBeyondReceiptLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "custodian.txt", "d7", nil)
	survivor, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "A. Lovelace", "operator")
	require.NoError(t, err)
	// Each UUID-length assignment ID takes 38 receipt bytes with its quotes.
	_, err = s.db.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<?
	) INSERT INTO custodian_assignments(assignment_id,scope_kind,node_id,content_version_id,person_id,raw_label,raw_label_folded,rank,basis,source_ref,recorded_at)
	SELECT printf('00000000-0000-4000-8000-%012d',value),'document',?,?,?,'Synthetic','synthetic','additional','operator_assigned','test',? FROM seq`,
		document.MaxPersonMergeMovedBytes/38+1, version.NodeID, version.ID, absorbed.PersonID, nowRFC3339())
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.ErrorIs(t, err, ErrPersonMergeTooLarge)
	resolved, through, err := s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	require.Equal(t, absorbed.PersonID, resolved.PersonID)
	require.Empty(t, through, "a rejected merge leaves the absorbed person in place")
}

func TestMergeReceiptKeepsExternalTupleComponentsDistinct(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: absorbed.PersonID,
		System: "msgvault", ArchiveID: "archive/part", UID: "uid/part", UIDKind: "vcard_uid", UIDState: "current"}, absorbed.Revision)
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	receipt, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Equal(t, []PersonExternalUID{{System: "msgvault", ArchiveID: "archive/part", UID: "uid/part"}}, receipt.Moved.ExternalUIDs)
}

func survivorAliasID(t *testing.T, s *Store, personID string) string {
	t.Helper()
	identities, err := s.PersonIdentities(t.Context(), personID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	return identities[0].IdentityID
}

func TestMergePersonsRejectsConflictingCurrentExternalUIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	left, err := s.CreatePerson(t.Context(), "Left", "operator")
	require.NoError(t, err)
	right, err := s.CreatePerson(t.Context(), "Right", "operator")
	require.NoError(t, err)
	for person, uid := range map[Person]string{left: "left", right: "right"} {
		_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
			ArchiveID: "synthetic", UID: uid, UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
		require.NoError(t, err)
	}
	left, _, err = s.PersonByID(t.Context(), left.PersonID)
	require.NoError(t, err)
	right, _, err = s.PersonByID(t.Context(), right.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), left.PersonID, right.PersonID, operationID, left.Revision, right.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergePersonsPreservesExternalUIDTransitions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, survivorUID, absorbedUID string
		multihop                       bool
	}{
		{"direct", "current", "retired", false},
		{"multihop to survivor", "current", "retired", true},
		{"multihop to absorbed", "retired", "current", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			survivor, err := s.CreatePerson(t.Context(), "Ada", "operator")
			require.NoError(t, err)
			absorbed, err := s.CreatePerson(t.Context(), "Ada Old", "operator")
			require.NoError(t, err)
			for person, uid := range map[Person]string{survivor: test.survivorUID, absorbed: test.absorbedUID} {
				_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
					ArchiveID: "synthetic", UID: uid, UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
				require.NoError(t, err)
			}
			if test.multihop {
				require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "middle", []string{"retired"}))
				require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "current", []string{"middle"}))
			} else {
				require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "current", []string{"retired"}))
			}
			survivor, _, err = s.PersonByID(t.Context(), survivor.PersonID)
			require.NoError(t, err)
			absorbed, _, err = s.PersonByID(t.Context(), absorbed.PersonID)
			require.NoError(t, err)
			operationID, err := newUUIDv4()
			require.NoError(t, err)
			_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
			require.NoError(t, err)
			identities, err := s.PersonExternalIdentities(t.Context(), survivor.PersonID)
			require.NoError(t, err)
			require.Len(t, identities, 2)
			states := map[string]string{}
			for _, identity := range identities {
				states[identity.UID] = identity.UIDState
			}
			require.Equal(t, "current", states["current"])
			require.Equal(t, "retired", states["retired"])
		})
	}
}

func TestMergePersonsRewritesAliasChainsToOneHop(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	first, err := s.CreatePerson(t.Context(), "First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(t.Context(), "Second", "operator")
	require.NoError(t, err)
	third, err := s.CreatePerson(t.Context(), "Third", "operator")
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), second.PersonID, first.PersonID, operationID, second.Revision, first.Revision)
	require.NoError(t, err)
	second, _, err = s.PersonByID(t.Context(), second.PersonID)
	require.NoError(t, err)
	operationID, err = newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), third.PersonID, second.PersonID, operationID, third.Revision, second.Revision)
	require.NoError(t, err)
	resolved, through, err := s.PersonByID(t.Context(), first.PersonID)
	require.NoError(t, err)
	require.Equal(t, third.PersonID, resolved.PersonID)
	require.Equal(t, first.PersonID, through)
}

func TestSplitPersonMovesOnlyExplicitIdentitiesAndReplays(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	source, first := addTestPersonIdentity(t, s, source, "email", "Ada@example.test", "first")
	source, second := addTestPersonIdentity(t, s, source, "email", "other@example.test", "second")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	request := PersonSplitRequest{PersonID: source.PersonID, OperationID: operationID, DisplayName: "Ada Two",
		Revision: source.Revision, IdentityIDs: []string{second.IdentityID}}
	receipt, err := s.SplitPerson(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, []string{second.IdentityID}, receipt.MovedIdentityIDs)
	remaining, err := s.PersonIdentities(t.Context(), source.PersonID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, first.IdentityID, remaining[0].IdentityID)
	moved, err := s.PersonIdentities(t.Context(), receipt.NewPersonID)
	require.NoError(t, err)
	require.Len(t, moved, 1)
	replayed, err := s.SplitPerson(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	request.AssignmentIDs = []string{}
	request.External = []PersonExternalUID{}
	replayed, err = s.SplitPerson(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
}

func TestSplitPersonMovesCustodianAndExternalUID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreatePerson(ctx, "Source", "operator")
	require.NoError(t, err)
	external, err := s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: source.PersonID,
		System: "msgvault", ArchiveID: "example-archive", UID: "split-uid", UIDKind: "vcard_uid", UIDState: "current"}, source.Revision)
	require.NoError(t, err)
	nodeID, versionID := seedPeopleVersion(t, s)
	assignment, err := s.SetCustodian(ctx, CustodianRequest{
		Scope:    CustodianScope{Kind: "document", NodeID: nodeID, ContentVersionID: versionID},
		PersonID: source.PersonID, RawLabel: "Source", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	source, _, err = s.PersonByID(ctx, source.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	request := PersonSplitRequest{PersonID: source.PersonID, OperationID: operationID, DisplayName: "Split",
		Revision: source.Revision, IdentityIDs: []string{}, AssignmentIDs: []string{assignment.AssignmentID}, External: []PersonExternalUID{{System: external.System, ArchiveID: external.ArchiveID, UID: external.UID}}}
	receipt, err := s.SplitPerson(ctx, request)
	require.NoError(t, err)
	resolved, err := s.ResolveExternalPersonUID(ctx, external.System, external.ArchiveID, external.UID)
	require.NoError(t, err)
	require.Equal(t, receipt.NewPersonID, resolved.PersonID)
	assignments, err := s.CustodiansForVersion(ctx, versionID)
	require.NoError(t, err)
	require.Len(t, assignments, 1)
	require.Equal(t, receipt.NewPersonID, *assignments[0].PersonID)
	require.ErrorIs(t, s.RetireCustodian(ctx, assignment.AssignmentID, assignment.Revision), ErrStaleRevision)
	// Rebuild the selector from the identity after its owner and timestamps changed.
	moved, err := s.PersonExternalIdentities(ctx, receipt.NewPersonID)
	require.NoError(t, err)
	require.Len(t, moved, 1)
	request.External = []PersonExternalUID{{System: moved[0].System, ArchiveID: moved[0].ArchiveID, UID: moved[0].UID}}
	replayed, err := s.SplitPerson(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	request.External[0].UID = "different-uid"
	_, err = s.SplitPerson(ctx, request)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
	// Merging the split person back must fence the assignment a second time.
	source, _, err = s.PersonByID(ctx, source.PersonID)
	require.NoError(t, err)
	operationID, err = newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, source.PersonID, receipt.NewPersonID, operationID, source.Revision, 1)
	require.NoError(t, err)
	require.ErrorIs(t, s.RetireCustodian(ctx, assignment.AssignmentID, assignments[0].Revision), ErrStaleRevision)
}

func TestPersonSplitMissingAssertion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	_, err = s.SplitPerson(t.Context(), PersonSplitRequest{PersonID: p.PersonID, OperationID: uuid.NewString(), DisplayName: "Grace", Revision: p.Revision, AssertionIDs: []string{uuid.NewString()}})
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergePersonsCarriesKeyFaceIntoPortrait(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Absorbed", "operator")
	require.NoError(t, err)
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("portrait"), "image/jpeg")
	// The key face is smaller than the fallback, so area ranking cannot pick it.
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Width: .5, Height: .5}, survivor.PersonID)
	require.NoError(t, err)
	key, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .7, Width: .1, Height: .1}, absorbed.PersonID)
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(ctx, absorbed.PersonID)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{Action: "key", Revision: key.Revision, PersonID: absorbed.PersonID, PersonRevision: absorbed.Revision, FaceID: key.FaceID})
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(ctx, absorbed.PersonID)
	require.NoError(t, err)
	survivor, _, err = s.PersonByID(ctx, survivor.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.NewString(), survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	detail, err := s.PersonDetailPage(ctx, survivor.PersonID, PersonAssertionPage{})
	require.NoError(t, err)
	require.Equal(t, key.FaceID, detail.KeyFaceID)
	rows, err := s.PeopleByDisplayName(ctx, "Survivor", "", "", 10, true, PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.InDelta(t, .7, rows[0].PhotoSummary.Face.Box.X, 1e-9)
}
