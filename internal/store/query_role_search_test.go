package store

import (
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/query"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoleSearchOperandSets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	email := seedCurrentRoleSearchEvent(t, s, "email.txt", "a1", nil)
	pdf := seedCurrentRoleSearchEvent(t, s, "paper.pdf", "a2", nil)
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
			if tc.text == `bcc:"Dahlia Blind"` {
				require.Equal(t, []string{"blind_copy"}, projection.Rows[0].MatchedRoles)
			}
		})
	}
	structured := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["sender","organizer"]},{"person_id":%q,"roles":["recipient"]}]}}`, a.PersonID, b.PersonID))
	selected, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: structured})
	require.NoError(t, err)
	require.Len(t, selected.Rows, 1)
	require.Equal(t, "email.txt", selected.Rows[0].Name)
	forbidden := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["blind_copy"]}]}}`, a.PersonID))
	_, err = s.MaterializeQuerySnapshot(WithSafeRoleSearch(ctx), SnapshotRequest{Query: forbidden})
	require.ErrorContains(t, err, "Bcc searches are unavailable here.")
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"between:(\"Aster Sender\", \"Dahlia Blind\")"}`)
	result, err := s.MaterializeQuerySnapshot(WithSafeRoleSearch(ctx), SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
}

func TestRoleSearchMatchesAndPeopleFacet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "message.txt", "b1", nil)
	person, err := s.CreatePerson(ctx, "Indigo Sender", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ id, role string }{{person.PersonID, "sender"}, {person.PersonID, "speaker"}} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: edge.id, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	for _, role := range []string{"author", "last_saved_by", "custodian", "recipient", "copied", "organizer", "attendee", "participant"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	photo := browsePhotoNode(t, s, "portrait.txt", fakeHash("fac1"), "text/plain")
	for _, role := range []string{"sender", "participant"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: photo.CurrentVersionID, PersonID: person.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	publishRoleFixtures(t, s)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Indigo Sender\""}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 2)
	for _, row := range projection.Rows {
		switch row.Name {
		case "message.txt":
			require.Equal(t, []string{"organizer", "sender"}, row.MatchedRoles)
		case "portrait.txt":
			require.Equal(t, []string{"sender"}, row.MatchedRoles)
		default:
			t.Fatalf("unexpected row %q", row.Name)
		}
	}
	require.Len(t, projection.Facets, 1)
	require.True(t, projection.Facets[0].Available)
	require.Len(t, projection.Facets[0].Values, 1)
	require.Equal(t, person.PersonID, projection.Facets[0].Values[0].Key)
	require.ElementsMatch(t, []string{"author", "last_saved_by", "custodian", "sender", "recipient", "copied", "organizer", "attendee", "participant", "speaker"}, projection.Facets[0].Values[0].Roles)
	require.EqualValues(t, 2, projection.Facets[0].Values[0].Count)
}

func checkRoleSearchCurrentPersonRelation(t *testing.T) {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "relation.txt", "c1", nil)
	direct, err := s.CreatePerson(ctx, "Direct Relation", "operator")
	require.NoError(t, err)
	for _, role := range []string{"blind_copy", "speaker"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: direct.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	check := func(safe bool, person, role string, want bool) {
		relation := currentDocumentPersonEdgesSQL("cv.version_id", safe)
		statement := `SELECT EXISTS(SELECT 1 FROM ` + relation + ` edge WHERE edge.person_id=? AND edge.role=?) FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE n.id=?`
		var found bool
		state, err := effectiveDocumentEventState(ctx, s.db, DocumentEventsDeriverFingerprint)
		require.NoError(t, err)
		err = s.db.QueryRowContext(ctx, statement, document.PersonResolverFingerprint(), state.InputEpoch, state.ContractVersion, state.DeriverFingerprint, person, role, version.NodeID).Scan(&found)
		require.NoError(t, err)
		require.Equal(t, want, found)
	}
	published := publication.People.Edges[0]
	check(false, published.PersonID, published.Role, true)
	check(false, direct.PersonID, "blind_copy", true)
	check(true, direct.PersonID, "blind_copy", false)
	check(true, direct.PersonID, "speaker", true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET input_epoch=input_epoch+1`)
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, false)
	check(false, direct.PersonID, "blind_copy", true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET input_epoch=input_epoch-1`)
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_attempts SET state='failed' WHERE content_version_id=$1`, version.ID)
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, false)
	check(false, direct.PersonID, "blind_copy", true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_attempts SET state='indexed' WHERE content_version_id=$1`, version.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, false)
	check(false, direct.PersonID, "blind_copy", true)
	check(true, direct.PersonID, "speaker", true)
}

func TestRoleSearchMentionNamesAreLiteral(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Jory Pellham", "operator")
	require.NoError(t, err)
	person, _ = addTestPersonIdentity(t, s, person, "name_alias", `A "OR" *`, "quoted-alias")
	_, err = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Derived Alias", Origin: "derived",
		EvidenceKind: "source_metadata", EvidenceID: "derived-alias", Confidence: "name_candidate",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	for i := range 198 {
		person, _ = addTestPersonIdentity(t, s, person, "name_alias", fmt.Sprintf("Alias %02d", i), fmt.Sprintf("alias-%02d", i))
	}
	names, err := (queryResolver{q: s.db}).personMentionNames(ctx, person.PersonID)
	require.NoError(t, err)
	require.Len(t, names, 201)
	require.Equal(t, "Jory Pellham", names[0])
	require.Contains(t, names, `A "OR" *`)
	require.Contains(t, names, "Derived Alias")
	compiled, err := s.CompileQuery(ctx, compilerQuery(t, `mentions:"Jory Pellham"`))
	require.NoError(t, err)
	_, args, err := compiled.Bind("")
	require.NoError(t, err)
	require.Contains(t, args[1], `"A ""OR"" *"`)
	require.Contains(t, args[1], `"Alias 197"`)
}

func TestRoleSearchNegationWithPendingPeople(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "sender.txt", "a3", []document.DocumentEventActorV1{{
		Role: "sender", ActorKey: "email:sender@example.test", Address: "sender@example.test", DisplayName: "Sender Person", Claim: `"sender"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	negative := snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Sender Person\""}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: negative})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	orQuery := snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Sender Person\" OR name:sender"}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: orQuery})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	_, err = s.CreateSavedQuery(ctx, "Pending role", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"from:\"Sender Person\""}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Nested pending role", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Pending role\""}`))
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT saved:\"Nested pending role\""}`)})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	_, err = s.CreateSavedQuery(ctx, "Collapsed sender", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"from:\"Sender Person\"","filters":{"collapse_duplicates":true}}`))
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT saved:\"Collapsed sender\""}`)})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	person := input.Bindings[input.Actors[0].ActorKey][0]
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Sender Person\""}`)})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	_, err = s.CreatePerson(ctx, "Absent Person", "operator")
	require.NoError(t, err)
	input, err = s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Absent Person\""}`)})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
}

func TestRoleSearchPeopleFacetIncomplete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedCurrentRoleSearchEvent(t, s, "pending.txt", "a5", nil)
	result, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, result.Facets, 1)
	require.False(t, result.Facets[0].Available)
	require.Equal(t, "people_evidence_incomplete", result.Facets[0].Reason)
	require.Nil(t, result.Facets[0].Missing)
	publishRoleFixtures(t, s)
	result, err = s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.True(t, result.Facets[0].Available)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_event_attempts SET state='failed' WHERE content_version_id=$1`, version.ID)
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.False(t, result.Facets[0].Available)
	require.Equal(t, "people_evidence_incomplete", result.Facets[0].Reason)
}

func TestRoleSearchPeopleFacetRetainsExpressionAndPersonIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	first := seedCurrentRoleSearchEvent(t, s, "keep-one.txt", "f101", nil)
	second := seedCurrentRoleSearchEvent(t, s, "keep-two.txt", "f102", nil)
	third := seedCurrentRoleSearchEvent(t, s, "outside.txt", "f103", nil)
	anchor, err := s.CreatePerson(ctx, "Facet Anchor", "operator")
	require.NoError(t, err)
	selected, err := s.CreatePerson(ctx, "Facet Selected", "operator")
	require.NoError(t, err)
	other, err := s.CreatePerson(ctx, "Facet Other", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, person, role string }{
		{first.ID, anchor.PersonID, "sender"}, {first.ID, other.PersonID, "recipient"},
		{second.ID, selected.PersonID, "author"}, {third.ID, anchor.PersonID, "sender"},
	} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: edge.person, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	for i := range 55 {
		p, err := s.CreatePerson(ctx, fmt.Sprintf("Person %02d", i), "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: first.ID, PersonID: p.PersonID, Role: "attendee", Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	publishRoleFixtures(t, s)
	value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"name:keep AND NOT path:/missing","filters":{"person_ids":[%q],"people":[{"person_id":%q,"roles":["author"]}]}}`, anchor.PersonID, selected.PersonID))
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Empty(t, projection.Rows)
	facet := facetByDimension(t, projection, "person_role")
	require.EqualValues(t, 1, *facet.Total)
	require.Len(t, facet.Values, 51)
	require.EqualValues(t, 7, *facet.Other)
	require.EqualValues(t, 1, facetCounts(facet)[anchor.PersonID])
	require.EqualValues(t, 1, facetCounts(facet)[other.PersonID])
	last := facet.Values[50]
	require.Equal(t, selected.PersonID, last.Key)
	require.True(t, last.Selected)
	require.Zero(t, last.Count)
	require.Equal(t, []string{"author"}, last.Roles)
}

func TestRoleSearchBetweenCurrentVersionAndMergedPeople(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	old := seedCurrentRoleSearchEvent(t, s, "versions.txt", "e1", nil)
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

func TestPublishedBccActorQueryPrivacy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "bcc.txt", "f1", []document.DocumentEventActorV1{{
		Role: "blind_copy", ActorKey: "email:bcc@example.test", Address: "bcc@example.test", DisplayName: "Bcc Person", Claim: `"recipient"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	publication.People.Edges[0].Sensitive = true // The processing resolver marks every blind-copy edge sensitive.
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	id := publication.People.Edges[0].PersonID
	for _, operand := range []string{"person", "to", "bcc"} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, operand+`:"Bcc Person"`))
		result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		if operand != "person" {
			require.Equal(t, []string{"blind_copy"}, result.Rows[0].MatchedRoles)
		}
		safe, err := s.MaterializeQuerySnapshot(WithSafeRoleSearch(ctx), SnapshotRequest{Query: value})
		if operand == "bcc" {
			require.ErrorContains(t, err, "bcc searches are unavailable here")
		} else {
			require.NoError(t, err)
			if operand == "person" {
				require.Len(t, safe.Rows, 1)
			} else {
				require.Empty(t, safe.Rows)
			}
		}
	}

	sensitive := seedCurrentRoleSearchEvent(t, s, "sensitive-author.txt", "f3", []document.DocumentEventActorV1{{
		Role: "author", ActorKey: "email:sensitive@example.test", Address: "sensitive@example.test", DisplayName: "Sensitive Author", Claim: "Sensitive Author", Sensitive: true,
	}})
	input, err = s.PrepareDocumentPeopleInputs(ctx, sensitive.ID)
	require.NoError(t, err)
	publication = documentPeoplePublicationForInput(t, input)
	publication.People.Edges[0].Sensitive = true
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	sensitiveQuery := snapshotTestQuery(t, `{"syntax":"advanced","text":"author:\"Sensitive Author\""}`)
	fullResult, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: sensitiveQuery})
	require.NoError(t, err)
	require.Len(t, fullResult.Rows, 1)
	safeResult, err := s.MaterializeQuerySnapshot(WithSafeRoleSearch(ctx), SnapshotRequest{Query: sensitiveQuery})
	require.NoError(t, err)
	require.Empty(t, safeResult.Rows)

	other := seedCurrentRoleSearchEvent(t, s, "visible.txt", "f4", nil)
	visible, err := s.CreatePerson(ctx, "Visible Person", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, role string }{{version.ID, "blind_copy"}, {other.ID, "sender"}} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: visible.PersonID, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	publishRoleFixtures(t, s)
	input, err = s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication = documentPeoplePublicationForInput(t, input)
	publication.People.Edges[0].Sensitive = true
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"name:bcc","filters":{"people":[{"person_id":%q,"roles":["blind_copy"]},{"person_id":%q,"roles":["blind_copy"]}]}}`, visible.PersonID, id))
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, result.Facets, 1)
	facet := result.Facets[0]
	require.True(t, facet.Available)
	require.EqualValues(t, 1, *facet.Total)
	require.EqualValues(t, 1, *facet.Missing)
	require.Zero(t, *facet.Other)
	require.Len(t, facet.Values, 1)
	require.Equal(t, visible.PersonID, facet.Values[0].Key)
	require.Equal(t, "Visible Person", facet.Values[0].Label)
	require.True(t, facet.Values[0].Selected)
	require.Zero(t, facet.Values[0].Count)
	require.Empty(t, facet.Values[0].Roles)
}

func TestRoleSearchSavedRunPreservesMergedDefinitionReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "saved-merged.txt", "f2", nil)
	survivor, err := s.CreatePerson(ctx, "Saved Survivor", "operator")
	require.NoError(t, err)
	retired, err := s.CreatePerson(ctx, "Saved Retired", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: retired.PersonID, Role: "speaker", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: retired.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	payload := fmt.Appendf(nil, `{"filters":{"people":[{"person_id":%q,"roles":["speaker"]},{"person_id":%q,"roles":["author"]},{"person_id":%q,"roles":["speaker"]}]}}`, retired.PersonID, retired.PersonID, survivor.PersonID)
	definition, err := s.CreateSavedQuery(ctx, "Merged person", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, retired.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, retired.Revision)
	require.NoError(t, err)
	publishRoleFixtures(t, s)
	value := snapshotTestQuery(t, string(payload))
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, value.Filters.People, result.Query.Filters.People)
	require.Equal(t, []string{"author", "speaker"}, result.Rows[0].MatchedRoles)
	require.Equal(t, survivor.PersonID, result.Facets[0].Values[0].Key)
	require.True(t, result.Facets[0].Values[0].Selected)
	wantIDs := []string{retired.PersonID, survivor.PersonID}
	slices.Sort(wantIDs)
	require.Equal(t, wantIDs, result.Facets[0].Values[0].SelectedPersonIDs)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	run, page, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, SnapshotRequest{Facets: []string{"person_role"}}, nil)
	require.NoError(t, err)
	require.Equal(t, definition.Fingerprint, run.QueryFingerprint)
	fingerprint, err := query.Fingerprint(page.Query)
	require.NoError(t, err)
	require.Equal(t, fingerprint, page.QueryFingerprint)
	require.Equal(t, value.Filters.People, page.Query.Filters.People)
	next, _, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, SnapshotRequest{}, nil)
	require.NoError(t, err)
	require.False(t, next.Comparison.DefinitionChanged)
}

func TestRoleSearchSupports64PeopleFilters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "capacity.txt", "ab3", nil)
	collection, err := s.BeginIngest(ctx, "cli", "Capacity collection")
	require.NoError(t, err)
	addCollectionMembership(t, s, collection, version.NodeID, "/capacity.txt", nil)
	label := "Capacity collection"
	_, err = s.SetCollectionLabel(ctx, collection.ID(), 1, &label)
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Capacity saved", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"collection:\"Capacity collection\""}`))
	require.NoError(t, err)
	roles := `["author","last_saved_by","custodian","sender","recipient","copied","organizer","attendee","participant","speaker","depicted"]`
	entries := make([]string, 64)
	for i := range entries {
		person, err := s.CreatePerson(ctx, fmt.Sprintf("Capacity Person %02d", i), "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "speaker", Action: "assert", Revision: 1})
		require.NoError(t, err)
		entries[i] = fmt.Sprintf(`{"person_id":%q,"roles":%s}`, person.PersonID, roles)
	}
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:\"Capacity saved\"","filters":{"people":[`+strings.Join(entries, ",")+`]}}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, []string{"speaker"}, result.Rows[0].MatchedRoles)
}

// The facet member budget bounds counts for one document with many people.
func TestRoleSearchPeopleFacetMembershipBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "budget.txt", "aabb", nil)
	first, err := s.CreatePerson(ctx, "Budget First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(ctx, "Budget Second", "operator")
	require.NoError(t, err)
	for _, role := range []string{"author", "sender"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: first.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	publishRoleFixtures(t, s)
	request := SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}}
	one, err := s.materializeQuerySnapshot(ctx, request, snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	require.True(t, one.Facets[0].Available)
	require.EqualValues(t, 1, *one.Facets[0].Total)
	require.ElementsMatch(t, []string{"author", "sender"}, one.Facets[0].Values[0].Roles)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: second.PersonID, Role: "recipient", Action: "assert", Revision: 1})
	require.NoError(t, err)
	publishRoleFixtures(t, s)
	limited, err := s.materializeQuerySnapshot(ctx, request, snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	require.False(t, limited.Facets[0].Available)
	require.Equal(t, "member_budget_exceeded", limited.Facets[0].Reason)
	exact, err := s.materializeQuerySnapshot(ctx, request, snapshotMaterializeOptions{FacetMemberLimit: 2})
	require.NoError(t, err)
	require.True(t, exact.Facets[0].Available)
	require.EqualValues(t, 1, *exact.Facets[0].Total)
	require.Len(t, exact.Facets[0].Values, 2)
}

func TestRoleSearchNegationBranchMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first := seedCurrentRoleSearchEvent(t, s, "branch-one.txt", "ccdd", nil)
	second := seedCurrentRoleSearchEvent(t, s, "branch-two.txt", "ccdd", nil)
	a, err := s.CreatePerson(ctx, "Branch Alpha", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Branch Beta", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, person, role string }{{first.ID, a.PersonID, "sender"}, {second.ID, b.PersonID, "recipient"}} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: edge.person, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	_, err = s.CreateSavedQuery(ctx, "Branch saved", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"from:\"Branch Alpha\""}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Branch outer", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Branch saved\""}`))
	require.NoError(t, err)
	publishRoleFixtures(t, s)
	for _, tc := range []struct {
		text  string
		roles []string
	}{
		{`from:"Branch Alpha" OR to:"Branch Beta"`, []string{"sender", "recipient"}},
		{`(from:"Branch Alpha" AND name:missing) OR name:branch-two`, nil},
		{`(from:"Branch Alpha" AND name:branch-one) OR (to:"Branch Beta" AND name:missing)`, []string{"sender"}},
		{`saved:"Branch outer"`, []string{"sender"}},
		{`name:branch-one AND NOT attendee:"Branch Alpha"`, nil},
		{`NOT from:"Branch Alpha"`, nil},
		{`NOT NOT from:"Branch Alpha"`, []string{"sender"}},
		{`NOT (NOT from:"Branch Alpha" OR to:"Branch Beta") OR name:branch-two`, []string{"sender"}},
		{`NOT (NOT from:"Branch Alpha" AND to:"Branch Beta") OR name:missing`, []string{"sender"}},
		{`NOT NOT saved:"Branch saved" OR to:"Branch Beta"`, []string{"sender", "recipient"}},
		{`NOT NOT between:("Branch Alpha", "Branch Beta") OR to:"Branch Beta"`, []string{"recipient"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"collapse_duplicates":true}}`, tc.text))
			result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
			require.NoError(t, err)
			require.Len(t, result.Rows, 1)
			require.ElementsMatch(t, tc.roles, result.Rows[0].MatchedRoles)
		})
	}
}

func TestRoleSearchHiddenPersonScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hidden := browsePhotoNode(t, s, "saved-hidden.jpg", fakeHash("saved-hidden"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Hidden Depicted", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: hidden.CurrentVersionID, PersonID: person.PersonID, Role: "depicted", Action: "assert", Revision: 1})
	require.NoError(t, err)
	public := browsePhotoNode(t, s, "open.txt", fakeHash("open"), "text/plain")
	visible, err := s.CreatePerson(ctx, "Visible Sender", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: public.CurrentVersionID, PersonID: visible.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	publishRoleFixtures(t, s)
	definition, err := s.CreateSavedQuery(ctx, "Hidden saved", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"person:\"Hidden Depicted\""}`))
	require.NoError(t, err)
	structured, err := s.CreateSavedQuery(ctx, "Hidden structured", "", SavedQueryKindQuery, fmt.Appendf(nil, `{"filters":{"people":[{"person_id":%q,"roles":["depicted"]}]}}`, person.PersonID))
	require.NoError(t, err)
	nested, err := s.CreateSavedQuery(ctx, "Hidden nested", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Hidden structured\""}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	safe := WithSafeRoleSearch(ctx)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Visible Sender\""}`)
	projection, err := s.MaterializeQuerySnapshot(safe, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 1)
	require.Equal(t, public.ID, projection.Rows[0].NodeID)
	projection, err = s.MaterializeQuerySnapshot(safe, SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.True(t, projection.Facets[0].Available)
	var facetPeople []string
	for _, facet := range projection.Facets[0].Values {
		facetPeople = append(facetPeople, facet.Key)
	}
	require.Contains(t, facetPeople, visible.PersonID)
	require.NotContains(t, facetPeople, person.PersonID)
	for _, saved := range []SavedQuery{definition, structured, nested} {
		if saved.ID != definition.ID {
			_, page, err := service.RunSaved(safe, "owner", saved.ID, saved.Revision, SnapshotRequest{}, nil)
			require.NoError(t, err)
			require.Empty(t, page.Rows)
		}
		_, page, err := service.RunSaved(ctx, "owner", saved.ID, saved.Revision, SnapshotRequest{}, nil)
		require.NoError(t, err)
		require.Len(t, page.Rows, 1)
	}
}

func seedCurrentRoleSearchEvent(t *testing.T, s *Store, name, seed string, actors []document.DocumentEventActorV1) ContentVersion {
	t.Helper()
	return seedDocumentPeopleEvent(t, s, name, seed, actors, DocumentEventsDeriverFingerprint)
}

func publishRoleFixtures(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, DocumentEventsDeriverFingerprint))
	targets, err := s.MissingDocumentEventTargetsAfter(ctx, DocumentEventsDeriverFingerprint, "", 1000)
	require.NoError(t, err)
	for _, target := range targets {
		record := documentEventRecord(t, s.VaultID(), target.ContentVersionID, "aa")
		record.DocumentKind = DocumentEventKindForMIMEType(target.MIMEType)
		record.Events[0].Actors = nil
		raw, _, err := document.MarshalDocumentEventsV1(record)
		require.NoError(t, err)
		_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
		require.NoError(t, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version_id FROM content_versions`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, id := range ids {
		input, err := s.PrepareDocumentPeopleInputs(ctx, id)
		require.NoError(t, err)
		raw, err := canonical.Marshal(input)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(ctx, DocumentPeoplePublication{People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: id, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}}, InputsSHA256: fakeSHA256(raw), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch})
		require.NoError(t, err)
	}
}

func TestRoleSearchMentionsRequireDocumentContent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Linden Vale", "operator")
	require.NoError(t, err)
	_, _ = addTestPersonIdentity(t, s, person, "name_alias", "Willow Finch", "willow-finch")
	for i, name := range []string{"Linden Vale.txt", "Willow Finch.txt"} {
		node, err := s.CreateFile(ctx, s.RootID(), name, testSHA256([]byte(fmt.Sprintf("mention-name-%d", i))), 20, "text/plain")
		require.NoError(t, err)
		seedCompiledLegacyText(t, s, node, "unrelated indexed body")
	}
	body, err := s.CreateFile(ctx, s.RootID(), "neutral.txt", fakeHash("ce03"), 20, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, body, "A note for Willow Finch")
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Linden Vale\""}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, "neutral.txt", result.Rows[0].Name)
	require.Equal(t, []string{"mentioned"}, result.Rows[0].MatchedRoles)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Linden Vale\" OR name:Vale"}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	for _, row := range result.Rows {
		if row.Name == "Linden Vale.txt" {
			require.Empty(t, row.MatchedRoles)
		} else {
			require.Equal(t, []string{"mentioned"}, row.MatchedRoles)
		}
	}
	// FTS distinguishes the fullwidth display name from its ASCII alias.
	person, err = s.CreatePerson(ctx, "Ａda Lovelace", "operator")
	require.NoError(t, err)
	_, _ = addTestPersonIdentity(t, s, person, "name_alias", "Ada Lovelace", "ascii-alias")
	file, err := s.CreateFile(ctx, s.RootID(), "ascii.txt", testSHA256([]byte("Ada Lovelace")), 12, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, file, "Ada Lovelace")
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Ａda Lovelace\""}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, "ascii.txt", result.Rows[0].Name)
	person, err = s.CreatePerson(ctx, "Null\x00Name", "operator")
	require.NoError(t, err)
	_, _ = addTestPersonIdentity(t, s, person, "name_alias", "Searchable Alias", "searchable-alias")
	file, err = s.CreateFile(ctx, s.RootID(), "alias.txt", fakeHash("ce06"), 12, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, file, "Searchable Alias")
	file, err = s.CreateFile(ctx, s.RootID(), "fabricated.txt", fakeHash("ce07"), 12, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, file, "Null Name")
	value = snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"mentions:%s"}`, person.PersonID))
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, "alias.txt", result.Rows[0].Name)
	person, err = s.CreatePerson(ctx, "Only\x00Null", "operator")
	require.NoError(t, err)
	value = snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"mentions:%s"}`, person.PersonID))
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	survivor, err := s.CreatePerson(ctx, "Merge Survivor", "operator")
	require.NoError(t, err)
	retired, err := s.CreatePerson(ctx, "Merge Retired", "operator")
	require.NoError(t, err)
	survivor, _ = addTestPersonIdentity(t, s, survivor, "name_alias", "Survivor Alias", "survivor-alias")
	_, err = s.MergePersons(ctx, survivor.PersonID, retired.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, retired.Revision)
	require.NoError(t, err)
	for i, text := range []string{"Merge Retired", "Survivor Alias"} {
		file, err = s.CreateFile(ctx, s.RootID(), fmt.Sprintf("merged-%d.txt", i), fakeHash(fmt.Sprintf("ce1%d", i)), 12, "text/plain")
		require.NoError(t, err)
		seedCompiledLegacyText(t, s, file, text)
	}
	value = snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"mentions:%s"}`, retired.PersonID))
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	require.ElementsMatch(t, []string{"merged-0.txt", "merged-1.txt"}, []string{result.Rows[0].Name, result.Rows[1].Name})
}

func TestRoleSearchMentionsSelectedRendition(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Linden Vale", "operator")
	require.NoError(t, err)
	_, _ = addTestPersonIdentity(t, s, person, "name_alias", "Willow Finch", "willow-finch")
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "A note for Willow Finch")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("ce04"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `mentions:"Linden Vale"`, query.Filters{})
	require.NoError(t, s.withLexicalGenerationRead(ctx, func(q metadataQuerier, selected LexicalGeneration) error {
		require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, q, compiled, selected.ID))
		return nil
	}))
}

func TestRoleSearchSavedPredicatesAcrossRelations(t *testing.T) {
	t.Parallel()
	fixture := newSnapshotFacetFixture(t)
	ctx := t.Context()
	person, err := fixture.store.CreatePerson(ctx, "Prepared Sender", "operator")
	require.NoError(t, err)
	_, err = fixture.store.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: fixture.nodes["a"].CurrentVersionID, PersonID: person.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = fixture.store.CreateSavedQuery(ctx, "Prepared inner", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"from:\"Prepared Sender\"","filters":{"collapse_duplicates":true}}`))
	require.NoError(t, err)
	_, err = fixture.store.CreateSavedQuery(ctx, "Prepared outer", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Prepared inner\""}`))
	require.NoError(t, err)
	prefix := `(saved:"Prepared outer")`
	cases := []struct {
		text    string
		matched bool
	}{}
	for _, relation := range []string{`path:/missing`, `collection:Second`, `text_coverage:complete`, `has_duplicates:true`} {
		cases = append(cases, struct {
			text    string
			matched bool
		}{"(" + prefix + " AND name:missing) OR (" + relation + " AND name:a.PDF)", relation != `path:/missing`})
	}
	for _, tc := range cases {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"collapse_duplicates":true}}`, tc.text))
		result, err := fixture.store.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Coverage: fixture.selection})
		require.NoError(t, err, tc.text)
		if !tc.matched {
			require.Empty(t, result.Rows, tc.text)
			continue
		}
		require.Len(t, result.Rows, 1, tc.text)
		require.Equal(t, fixture.nodes["a"].ID, result.Rows[0].NodeID, tc.text)
		require.Equal(t, []string{"sender"}, result.Rows[0].MatchedRoles, tc.text)
	}
}
