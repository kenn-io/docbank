package store

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoleSearchOperandSets(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	email := seedDocumentPeopleEvent(t, s, "email.txt", "a1", nil)
	pdf := seedDocumentPeopleEvent(t, s, "paper.pdf", "a2", nil)
	a, err := s.CreatePerson(ctx, "Aster Sender", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Birch Recipient", "operator")
	require.NoError(t, err)
	c, err := s.CreatePerson(ctx, "Cedar Copy", "operator")
	require.NoError(t, err)
	d, err := s.CreatePerson(ctx, "Dahlia Blind", "operator")
	require.NoError(t, err)
	e, err := s.CreatePerson(ctx, "Elm Custodian", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, person, role string }{
		{email.ID, a.PersonID, "sender"}, {email.ID, b.PersonID, "recipient"}, {email.ID, c.PersonID, "copied"}, {email.ID, d.PersonID, "blind_copy"}, {pdf.ID, a.PersonID, "author"}, {pdf.ID, e.PersonID, "custodian"}, {email.ID, e.PersonID, "speaker"},
	} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: edge.person, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{`from:"Aster Sender"`, []string{"email.txt"}}, {`author:"Aster Sender"`, []string{"paper.pdf"}},
		{`to:"Cedar Copy"`, []string{"email.txt"}}, {`cc:"Birch Recipient"`, nil},
		{`bcc:"Dahlia Blind"`, []string{"email.txt"}}, {`between:("Aster Sender", "Birch Recipient")`, []string{"email.txt"}},
		{`between:("Aster Sender", "Elm Custodian")`, []string{"email.txt"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			q := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, tc.text))
			projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: q})
			require.NoError(t, err)
			var names []string
			for _, row := range projection.Rows {
				names = append(names, row.Name)
			}
			require.Equal(t, tc.want, names)
		})
	}
	structured := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["sender","organizer"]},{"person_id":%q,"roles":["recipient"]}]}}`, a.PersonID, b.PersonID))
	selected, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: structured})
	require.NoError(t, err)
	require.Len(t, selected.Rows, 1)
	require.Equal(t, "email.txt", selected.Rows[0].Name)
	fullBcc := snapshotTestQuery(t, `{"syntax":"advanced","text":"bcc:\"Dahlia Blind\""}`)
	bccResult, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: fullBcc})
	require.NoError(t, err)
	require.Len(t, bccResult.Rows, 1)
	require.Equal(t, []string{"blind_copy"}, bccResult.Rows[0].MatchedRoles)
	forbidden := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["blind_copy"]}]}}`, a.PersonID))
	_, err = s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: forbidden})
	require.ErrorContains(t, err, "bcc: is not available in browser sessions")
	q := snapshotTestQuery(t, `{"syntax":"advanced","text":"person:\"Dahlia Blind\""}`)
	_, err = s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: q})
	require.ErrorContains(t, err, "unknown person reference")
	for _, text := range []string{`to:"Dahlia Blind"`, `bcc:"Dahlia Blind"`, `between:("Aster Sender", "Dahlia Blind")`} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, text))
		_, err = s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
		require.ErrorContains(t, err, "unknown person reference")
	}
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: pdf.ID, PersonID: d.PersonID, Role: "participant", Action: "assert", Revision: 1})
	require.NoError(t, err)
	projection, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: q})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 1)
	require.Equal(t, "paper.pdf", projection.Rows[0].Name)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"bcc:\"Dahlia Blind\""}`)
	_, err = s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
	require.ErrorContains(t, err, "bcc: is not available in browser sessions")
}

func TestRoleSearchFrozenMatchesAndPeopleFacet(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "message.txt", "b1", nil)
	person, err := s.CreatePerson(ctx, "Indigo Sender", "operator")
	require.NoError(t, err)
	hidden, err := s.CreatePerson(ctx, "Hidden Recipient", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ id, role string }{{person.PersonID, "sender"}, {person.PersonID, "speaker"}, {hidden.PersonID, "blind_copy"}} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: edge.id, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	for _, role := range []string{"author", "last_saved_by", "custodian", "recipient", "copied", "organizer", "attendee", "participant"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	photo := browsePhotoNode(t, s, "portrait.jpg", fakeHash("fac1"), "image/jpeg")
	for _, role := range []string{"sender", "depicted"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: photo.CurrentVersionID, PersonID: person.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Indigo Sender\""}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 2)
	for _, row := range projection.Rows {
		require.Equal(t, []string{"sender"}, row.MatchedRoles)
	}
	require.Len(t, projection.Facets, 1)
	require.True(t, projection.Facets[0].Available)
	require.Len(t, projection.Facets[0].Values, 1)
	require.Equal(t, person.PersonID, projection.Facets[0].Values[0].Key)
	require.ElementsMatch(t, []string{"author", "last_saved_by", "custodian", "sender", "recipient", "copied", "organizer", "attendee", "participant", "speaker", "depicted"}, projection.Facets[0].Values[0].Roles)
	require.EqualValues(t, 2, projection.Facets[0].Values[0].Count)
}

// This exercises the shared edge relation on both catalog backends, not PostgreSQL search.
func checkRoleSearchCurrentPersonRelation(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "relation.txt", "c1", nil)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	direct, err := s.CreatePerson(ctx, "Direct Relation", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: direct.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	check := func(safe bool, person, role string, want bool) {
		relation := strings.ReplaceAll(currentDocumentPersonEdgesSQL("cv.version_id", safe, "1"), "?", "$1")
		var found bool
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+relation+` edge WHERE edge.person_id=$3 AND edge.role=$4) FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE n.id=$2`, document.PersonResolverFingerprint(), version.NodeID, person, role).Scan(&found)
		require.NoError(t, err)
		require.Equal(t, want, found)
	}
	published := publication.People.Edges[0]
	check(false, published.PersonID, string(published.Role), true)
	check(false, direct.PersonID, "blind_copy", true)
	check(true, direct.PersonID, "blind_copy", false)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: direct.PersonID, Role: "speaker", Action: "assert", Revision: 1})
	require.NoError(t, err)
	check(true, direct.PersonID, "speaker", true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	check(false, published.PersonID, string(published.Role), false)
	check(false, direct.PersonID, "blind_copy", true)
}

func TestRoleSearchMentionNamesAreLiteralAndBounded(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Jory Pellham", "operator")
	require.NoError(t, err)
	person, _ = addTestPersonIdentity(t, s, person, "name_alias", `A "OR" *`, "quoted-alias")
	person, derived := addTestPersonIdentity(t, s, person, "name_alias", "Hidden Derived Alias", "derived-alias")
	_, err = s.db.ExecContext(ctx, `UPDATE person_identities SET origin='derived' WHERE identity_id=?`, derived.IdentityID)
	require.NoError(t, err)
	visibleNames, err := (queryResolver{q: s.db}).personMentionNames(WithVisiblePeopleOnly(ctx), person.PersonID)
	require.NoError(t, err)
	require.Contains(t, visibleNames, `A "OR" *`)
	require.NotContains(t, visibleNames, "Hidden Derived Alias")
	fullNames, err := (queryResolver{q: s.db}).personMentionNames(ctx, person.PersonID)
	require.NoError(t, err)
	require.Contains(t, fullNames, "Hidden Derived Alias")
	for i := 0; i < 20; i++ {
		person, _ = addTestPersonIdentity(t, s, person, "name_alias", fmt.Sprintf("Alias %02d", i), fmt.Sprintf("alias-%02d", i))
	}
	names, err := (queryResolver{q: s.db}).personMentionNames(ctx, person.PersonID)
	require.NoError(t, err)
	require.Len(t, names, 16)
	require.Equal(t, "Jory Pellham", names[0])
	require.Contains(t, names, `A "OR" *`)
	compiled, err := s.CompileQuery(ctx, compilerQuery(t, `mentions:"Jory Pellham"`))
	require.NoError(t, err)
	_, args, err := compiled.Bind("")
	require.NoError(t, err)
	require.Contains(t, args, `"A ""OR"" *"`)
	require.Contains(t, compiled.PositiveTextTerms(), `A "OR" *`)
	require.Contains(t, compiled.PositiveTextTerms(), "Jory Pellham")
	require.Len(t, compiled.roleMatches, 1)
	require.Equal(t, "mentioned", compiled.roleMatches[0].label)
}

func TestRoleSearchPeopleFacetBoundsAndFreeze(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first := seedDocumentPeopleEvent(t, s, "meeting.txt", "d1", nil)
	second := seedDocumentPeopleEvent(t, s, "other.txt", "d2", nil)
	for i := 0; i < 25; i++ {
		p, err := s.CreatePerson(ctx, fmt.Sprintf("Person %02d", i), "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: first.ID, PersonID: p.PersonID, Role: "attendee", Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	absent, err := s.CreatePerson(ctx, "Absent Selection", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: second.ID, PersonID: absent.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"name:meeting","filters":{"people":[{"person_id":%q,"roles":["author"]}]}}`, absent.PersonID))
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Empty(t, projection.Rows)
	require.Len(t, projection.Facets[0].Values, 21)
	require.EqualValues(t, 5, *projection.Facets[0].Other)
	last := projection.Facets[0].Values[20]
	require.Equal(t, absent.PersonID, last.Key)
	require.True(t, last.Selected)
	require.Zero(t, last.Count)
	require.Equal(t, []string{"author"}, last.Roles)
	for i := range 49 {
		extra := seedDocumentPeopleEvent(t, s, fmt.Sprintf("extra-%02d.txt", i), fmt.Sprintf("%x", i+10000), nil)
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: extra.ID, PersonID: absent.PersonID, Role: "author", Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	frozen, err := service.Create(ctx, "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`), PageSize: 50, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.NotEmpty(t, frozen.NextCursor)
	require.Equal(t, "Absent Selection", frozen.Facets[0].Values[0].Label)
	_, err = s.UpdatePerson(ctx, absent.PersonID, absent.Revision, "Renamed Selection")
	require.NoError(t, err)
	reread, err := service.Page(ctx, "owner", frozen.SnapshotID, frozen.NextCursor)
	require.NoError(t, err)
	require.Equal(t, frozen.Facets, reread.Facets)
	require.Equal(t, frozen.SnapshotFingerprint, reread.SnapshotFingerprint)
	all := snapshotTestQuery(t, `{}`)
	limited, err := s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: all, Facets: []string{"person_role"}}, snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	require.False(t, limited.Facets[0].Available)
	require.Equal(t, "member_budget_exceeded", limited.Facets[0].Reason)
}

func TestRoleSearchBetweenCurrentVersionAndMergedPeople(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	old := seedDocumentPeopleEvent(t, s, "versions.txt", "e1", nil)
	a, err := s.CreatePerson(ctx, "First Person", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Second Person", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: old.ID, PersonID: a.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	node, err := s.NodeByID(ctx, old.NodeID)
	require.NoError(t, err)
	_, current, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("e2"), 20, "text/plain")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: current.ID, PersonID: b.PersonID, Role: "recipient", Action: "assert", Revision: 1})
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"between:(\"First Person\", \"Second Person\")"}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, "11111111-1111-4111-8111-111111111111", a.Revision, b.Revision)
	require.NoError(t, err)
	value = snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"between:(%s,%s)"}`, a.PersonID, b.PersonID))
	_, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.ErrorContains(t, err, "between: needs two different people")
}

func TestRoleSearchBrowserHiddenLock(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	public := browsePhotoNode(t, s, "open.txt", fakeHash("open"), "text/plain")
	hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("hidden"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Visible Sender", "operator")
	require.NoError(t, err)
	concealed, err := s.CreatePerson(ctx, "Hidden Sender", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, id string }{{public.CurrentVersionID, person.PersonID}, {hidden.CurrentVersionID, person.PersonID}, {hidden.CurrentVersionID, concealed.PersonID}} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: edge.id, Role: "sender", Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.NoError(t, s.LockPhotoHidden(ctx))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	visibleQuery := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Visible Sender\""}`)
	hiddenQuery := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Hidden Sender\""}`)
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		names       []string
		hiddenKnown bool
	}{
		{"locked", WithVisiblePeopleOnly(ctx), []string{"open.txt"}, false},
		{"unlocked", WithVisiblePeopleOnly(WithPhotoHiddenToken(ctx, token)), []string{"hidden.jpg", "open.txt"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := s.MaterializeQuerySnapshot(tc.ctx, SnapshotRequest{Query: visibleQuery, Facets: []string{"person_role"}})
			require.NoError(t, err)
			var names []string
			for _, row := range projection.Rows {
				names = append(names, row.Name)
			}
			require.ElementsMatch(t, tc.names, names)
			var facetPeople []string
			for _, value := range projection.Facets[0].Values {
				facetPeople = append(facetPeople, value.Key)
			}
			if tc.hiddenKnown {
				require.Contains(t, facetPeople, concealed.PersonID)
			} else {
				require.NotContains(t, facetPeople, concealed.PersonID)
			}
			_, err = s.MaterializeQuerySnapshot(tc.ctx, SnapshotRequest{Query: hiddenQuery})
			if tc.hiddenKnown {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "unknown person reference")
			}
		})
	}
}

func TestRoleSearchMergedFilterReturnsSurvivor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "merged.txt", "ef", nil)
	survivor, err := s.CreatePerson(ctx, "Survivor", "operator")
	require.NoError(t, err)
	retired, err := s.CreatePerson(ctx, "Retired", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: retired.PersonID, Role: "speaker", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, retired.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, retired.Revision)
	require.NoError(t, err)
	value := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["speaker"]}]}}`, retired.PersonID))
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, survivor.PersonID, result.Query.Filters.People[0].PersonID)
	require.Equal(t, survivor.PersonID, result.Facets[0].Values[0].Key)
	require.True(t, result.Facets[0].Values[0].Selected)
}

func TestPublishedBccActorIsNotVisibleToBrowserPeopleOperations(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "bcc.txt", "f1", []document.DocumentEventActorV1{{
		Role: "blind_copy", ActorKey: "email:bcc@example.test", Address: "bcc@example.test", DisplayName: "Bcc Person", Claim: `"recipient"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	id := publication.People.Edges[0].PersonID
	person, _, err := s.PersonByID(ctx, id)
	require.NoError(t, err)
	visible, _, err := s.PeopleByDisplayName(ctx, "Bcc Person", "", "", 100, true, PeopleSearchOptions{})
	require.NoError(t, err)
	require.Empty(t, visible)
	_, err = s.UpdatePerson(WithVisiblePeopleOnly(ctx), id, person.Revision, "New Name")
	require.ErrorIs(t, err, ErrNotFound)
	survivor, err := s.CreatePerson(ctx, "Public Person", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(WithVisiblePeopleOnly(ctx), survivor.PersonID, id, "11111111-1111-4111-8111-111111111111", survivor.Revision, person.Revision)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRoleSearchCollapsedDuplicateAnnotationsIncludeOtherMembers(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first := seedDocumentPeopleEvent(t, s, "one.txt", "fa", nil)
	second := seedDocumentPeopleEvent(t, s, "two.txt", "fa", nil)
	a, err := s.CreatePerson(ctx, "First Speaker", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Second Attendee", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: first.ID, PersonID: a.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: second.ID, PersonID: b.PersonID, Role: "attendee", Action: "assert", Revision: 1})
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"First Speaker\" OR attendee:\"Second Attendee\"","filters":{"collapse_duplicates":true}}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.ElementsMatch(t, []string{"sender", "attendee"}, result.Rows[0].MatchedRoles)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"(from:\"First Speaker\" AND name:missing) OR name:two","filters":{"collapse_duplicates":true}}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Empty(t, result.Rows[0].MatchedRoles)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"(from:\"First Speaker\" AND name:one) OR (attendee:\"Second Attendee\" AND name:missing)","filters":{"collapse_duplicates":true}}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, []string{"sender"}, result.Rows[0].MatchedRoles)
}
