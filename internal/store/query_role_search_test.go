package store

import (
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
		{email.ID, a.PersonID, "sender"}, {email.ID, b.PersonID, "recipient"}, {email.ID, c.PersonID, "copied"}, {email.ID, d.PersonID, "blind_copy"}, {pdf.ID, a.PersonID, "author"}, {pdf.ID, e.PersonID, "custodian"},
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
		{`between:("Aster Sender", "Elm Custodian")`, nil},
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
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Indigo Sender\""}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 1)
	require.Equal(t, []string{"sender"}, projection.Rows[0].MatchedRoles)
	require.Len(t, projection.Facets, 1)
	require.True(t, projection.Facets[0].Available)
	require.Len(t, projection.Facets[0].Values, 1)
	require.Equal(t, person.PersonID, projection.Facets[0].Values[0].Key)
	require.Equal(t, []string{"sender"}, projection.Facets[0].Values[0].Roles)
	require.EqualValues(t, 1, projection.Facets[0].Values[0].Count)
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
	fingerprint := projection.SnapshotFingerprint
	_, err = s.UpdatePerson(ctx, absent.PersonID, absent.Revision, "Renamed Selection")
	require.NoError(t, err)
	require.Equal(t, "Absent Selection", projection.Facets[0].Values[20].Label)
	require.Equal(t, fingerprint, projection.SnapshotFingerprint)
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
