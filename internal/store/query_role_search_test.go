package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoleSearchOperandSets(t *testing.T) {
	t.Parallel()
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
	require.ErrorContains(t, err, "bcc: is not available with safe people disclosure")
	q := snapshotTestQuery(t, `{"syntax":"advanced","text":"person:\"Dahlia Blind\""}`)
	visible, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: q})
	require.NoError(t, err)
	require.Len(t, visible.Rows, 1)
	for _, text := range []string{`to:"Dahlia Blind"`, `between:("Aster Sender", "Dahlia Blind")`} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, text))
		result, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
		require.NoError(t, err)
		require.Empty(t, result.Rows)
	}
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: pdf.ID, PersonID: d.PersonID, Role: "participant", Action: "assert", Revision: 1})
	require.NoError(t, err)
	projection, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: q})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 2)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"bcc:\"Dahlia Blind\""}`)
	_, err = s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
	require.ErrorContains(t, err, "bcc: is not available with safe people disclosure")
}

func TestRoleSearchFrozenMatchesAndPeopleFacet(t *testing.T) {
	t.Parallel()
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
		switch row.Name {
		case "message.txt":
			require.Equal(t, []string{"organizer", "sender"}, row.MatchedRoles)
		case "portrait.jpg":
			require.Equal(t, []string{"sender"}, row.MatchedRoles)
		default:
			t.Fatalf("unexpected row %q", row.Name)
		}
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
		relation := currentDocumentPersonEdgesSQL("cv.version_id", safe, "1")
		statement := `SELECT EXISTS(SELECT 1 FROM ` + relation + ` edge WHERE edge.person_id=? AND edge.role=?) FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE n.id=?`
		if s.postgresSchema != "" {
			for i := 1; i <= 7; i++ {
				statement = strings.Replace(statement, "?", fmt.Sprintf("$%d", i), 1)
			}
		}
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
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET deriver_fingerprint=$1`, fakeHash("ee"))
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, false)
	check(false, direct.PersonID, "blind_copy", true)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET deriver_fingerprint=$1`, DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	check(false, published.PersonID, published.Role, true)
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
	evidence := seedDocumentPeopleEvent(t, s, "visible.txt", "b2", nil)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: evidence.ID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	visibleNames, err := (queryResolver{q: s.db}).personMentionNames(WithVisiblePeopleOnly(ctx), person.PersonID)
	require.NoError(t, err)
	require.Contains(t, visibleNames, `A "OR" *`)
	require.Contains(t, visibleNames, "Derived Alias")
	fullNames, err := (queryResolver{q: s.db}).personMentionNames(ctx, person.PersonID)
	require.NoError(t, err)
	require.Contains(t, fullNames, "Derived Alias")
	for i := range 198 {
		person, _ = addTestPersonIdentity(t, s, person, "name_alias", fmt.Sprintf("Alias %02d", i), fmt.Sprintf("alias-%02d", i))
	}
	names, err := (queryResolver{q: s.db}).personMentionNames(ctx, person.PersonID)
	require.NoError(t, err)
	require.Len(t, names, 201)
	require.Equal(t, "Jory Pellham", names[0])
	require.Contains(t, names, `A "OR" *`)
	compiled, err := s.CompileQuery(ctx, compilerQuery(t, `mentions:"Jory Pellham"`))
	require.NoError(t, err)
	_, args, err := compiled.Bind("")
	require.NoError(t, err)
	require.Contains(t, args[1], `"A ""OR"" *"`)
	require.Contains(t, args[1], `"Alias 197"`)
	require.Contains(t, compiled.PositiveTextTerms(), `A "OR" *`)
	require.Contains(t, compiled.PositiveTextTerms(), "Jory Pellham")
	require.Len(t, compiled.roleMatches, 1)
	require.Equal(t, "mentioned", compiled.roleMatches[0].label)
	file, err := s.CreateFile(ctx, s.RootID(), "last-alias.txt", testSHA256([]byte("Alias 197")), 9, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, file, "Alias 197")
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Jory Pellham\""}`)})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, "last-alias.txt", result.Rows[0].Name)
	require.Equal(t, []string{"mentioned"}, result.Rows[0].MatchedRoles)
}

func TestRoleSearchMentionsFTSDistinctAlias(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ａda Lovelace", "operator")
	require.NoError(t, err)
	_, _ = addTestPersonIdentity(t, s, person, "name_alias", "Ada Lovelace", "ascii-alias")
	file, err := s.CreateFile(ctx, s.RootID(), "ascii.txt", testSHA256([]byte("Ada Lovelace")), 12, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, file, "Ada Lovelace")
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Ａda Lovelace\""}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, "ascii.txt", result.Rows[0].Name)
}

func TestRoleSearchNegationWithStaleEvents(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedCurrentRoleSearchEvent(t, s, "sender.txt", "a3", []document.DocumentEventActorV1{{
		Role: "sender", ActorKey: "email:sender@example.test", Address: "sender@example.test", DisplayName: "Sender Person", Claim: `"sender"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Sender Person\""}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET input_epoch=input_epoch+1`)
	require.NoError(t, err)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	other, err := s.CreatePerson(ctx, "Other Person", "operator")
	require.NoError(t, err)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Other Person\""}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Other Person\" OR name:sender"}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: other.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"from:\"Other Person\""}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	value = snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT from:\"Other Person\""}`)
	result, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
}

// Restored identity authority is visible to both callers even though no
// published name_alias document-person edge exists.
func TestRoleSearchMentionsRestoredDerivedAlias(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	person, err := source.CreatePerson(ctx, "Linden Vale", "operator")
	require.NoError(t, err)
	identity, err := source.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Willow Finch", Origin: "derived",
		EvidenceKind: "source_metadata", EvidenceID: "synthetic-name", Confidence: "name_candidate",
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	s := newTestStore(t)
	require.NoError(t, s.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	detail, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
	require.NoError(t, err)
	require.Contains(t, detail.Identities, identity)

	evidence := seedDocumentPeopleEvent(t, s, "evidence.txt", "b1", nil)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: evidence.ID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	aliasDoc, err := s.CreateFile(ctx, s.RootID(), "neutral.txt", testSHA256([]byte("restored-alias-body")), 20, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, aliasDoc, "A note for Willow Finch")
	other, err := s.CreateFile(ctx, s.RootID(), "other.txt", testSHA256([]byte("restored-other-body")), 20, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, other, "Unrelated body")
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"mentions:\"Linden Vale\""}`)
	full, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, full.Rows, 1)
	require.Equal(t, "neutral.txt", full.Rows[0].Name)
	require.Equal(t, []string{"mentioned"}, full.Rows[0].MatchedRoles)
	browser, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, browser.Rows, 1)
	require.Equal(t, full.Rows[0].NodeID, browser.Rows[0].NodeID)
	require.Equal(t, full.Rows[0].MatchedRoles, browser.Rows[0].MatchedRoles)
}

func TestRoleSearchPeopleFacetBoundsAndFreeze(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first := seedDocumentPeopleEvent(t, s, "meeting.txt", "d1", nil)
	second := seedDocumentPeopleEvent(t, s, "other.txt", "d2", nil)
	for i := range 25 {
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
	t.Parallel()
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
	t.Parallel()
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
		{"unlocked", WithVisiblePeopleOnly(WithPhotoHiddenToken(ctx, token)), []string{"open.txt"}, false},
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
			hiddenResult, err := s.MaterializeQuerySnapshot(tc.ctx, SnapshotRequest{Query: hiddenQuery})
			require.NoError(t, err)
			require.Empty(t, hiddenResult.Rows)
		})
	}
}

func TestRoleSearchMergedFilterReturnsSurvivor(t *testing.T) {
	t.Parallel()
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
		result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		if operand != "person" {
			require.Equal(t, []string{"blind_copy"}, result.Rows[0].MatchedRoles)
		}
		for _, facet := range result.Facets[0].Values {
			require.NotEqual(t, id, facet.Key)
			require.NotContains(t, facet.Roles, "blind_copy")
		}
		safe, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
		if operand == "bcc" {
			require.ErrorContains(t, err, "bcc: is not available with safe people disclosure")
		} else {
			require.NoError(t, err)
			if operand == "person" {
				require.Len(t, safe.Rows, 1)
			} else {
				require.Empty(t, safe.Rows)
			}
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE document_event_state SET input_epoch=input_epoch+1`)
	require.NoError(t, err)
	stale := snapshotTestQuery(t, `{"syntax":"advanced","text":"bcc:\"Bcc Person\""}`)
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: stale, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Empty(t, result.Rows)
	require.Empty(t, result.Facets[0].Values)
}

func TestRoleSearchCollapsedDuplicateAnnotationsIncludeOtherMembers(t *testing.T) {
	t.Parallel()
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

func TestRoleSearchSavedRunPreservesMergedDefinitionReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "saved-merged.txt", "f2", nil)
	survivor, err := s.CreatePerson(ctx, "Saved Survivor", "operator")
	require.NoError(t, err)
	retired, err := s.CreatePerson(ctx, "Saved Retired", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: retired.PersonID, Role: "speaker", Action: "assert", Revision: 1})
	require.NoError(t, err)
	payload := fmt.Appendf(nil, `{"filters":{"people":[{"person_id":%q,"roles":["speaker"]}]}}`, retired.PersonID)
	definition, err := s.CreateSavedQuery(ctx, "Merged person", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, retired.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, retired.Revision)
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	run, page, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, SnapshotRequest{Facets: []string{"person_role"}}, nil)
	require.NoError(t, err)
	require.Equal(t, definition.Fingerprint, run.QueryFingerprint)
	require.Equal(t, definition.Fingerprint, page.QueryFingerprint)
	require.Len(t, page.Rows, 1)
	require.Equal(t, retired.PersonID, page.Query.Filters.People[0].PersonID)
	require.Equal(t, survivor.PersonID, page.Facets[0].Values[0].Key)
}

func TestRoleSearchNestedSavedAndNegatedAttribution(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "nested.txt", "f3", nil)
	person, err := s.CreatePerson(ctx, "Nested Sender", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Inner roles", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"from:\"Nested Sender\""}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Outer roles", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Inner roles\""}`))
	require.NoError(t, err)
	for _, tc := range []struct {
		text  string
		roles []string
	}{
		{`saved:"Outer roles"`, []string{"sender"}},
		{`(saved:"Outer roles" AND name:missing) OR name:nested`, nil},
		{`name:nested AND NOT attendee:"Nested Sender"`, nil},
	} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, tc.text))
		result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		require.Equal(t, tc.roles, result.Rows[0].MatchedRoles)
	}
}

// The base query fits its compiler budget even though combining all role
// annotations with that population would exceed the statement bind budget.
func TestRoleSearchHighCardinalityAnnotation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "many-people.txt", "ab2", nil)
	roles := `["author","last_saved_by","custodian","sender","recipient","copied","organizer","attendee","participant","speaker","depicted"]`
	entries := make([]string, 64)
	for i := range entries {
		person, err := s.CreatePerson(ctx, fmt.Sprintf("Budget Person %02d", i), "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "speaker", Action: "assert", Revision: 1})
		require.NoError(t, err)
		entries[i] = fmt.Sprintf(`{"person_id":%q,"roles":%s}`, person.PersonID, roles)
	}
	value := snapshotTestQuery(t, `{"filters":{"people":[`+strings.Join(entries, ",")+`]}}`)
	compiled, err := s.CompileQuery(WithVisiblePeopleOnly(ctx), value)
	require.NoError(t, err)
	_, _, err = compiled.Bind("")
	require.NoError(t, err)
	result, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, []string{"speaker"}, result.Rows[0].MatchedRoles)
	collection, err := s.BeginIngest(ctx, "cli", "Synthetic people collection")
	require.NoError(t, err)
	addCollectionMembership(t, s, collection, version.NodeID, "/synthetic/many-people.txt", nil)
	label := "People collection"
	_, err = s.SetCollectionLabel(ctx, collection.ID(), 1, &label)
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(ctx, tx, profile, s.postgresSchema != "")
	}))
	selection := CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}
	for _, tc := range []struct {
		text    string
		matched bool
	}{
		{`collection:"People collection" AND text_coverage:unprocessed`, true},
		{`collection:"People collection" OR text_coverage:complete`, true},
		{`NOT collection:"People collection"`, false},
		{`collection:"People collection" AND text_coverage:complete`, false},
		{`text_coverage:unprocessed OR collection:"People collection"`, true},
		{`NOT text_coverage:complete AND collection:"People collection"`, true},
		{`NOT text_coverage:unprocessed`, false},
	} {
		mixed := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"people":[%s]}}`, tc.text, strings.Join(entries, ",")))
		got, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: mixed, Coverage: selection})
		require.NoError(t, err, tc.text)
		if !tc.matched {
			require.Empty(t, got.Rows, tc.text)
			continue
		}
		require.Len(t, got.Rows, 1, tc.text)
		require.Equal(t, version.NodeID, got.Rows[0].NodeID, tc.text)
		require.Equal(t, []string{"speaker"}, got.Rows[0].MatchedRoles, tc.text)
	}
	// Keep all 64 people and all 11 roles while relation leaves are evaluated
	// under each Boolean operator. The missing branch must not add chips.
	for _, text := range []string{
		`has_duplicates:false AND NOT path:/missing`,
		`path:/missing OR name:many-people`,
		`NOT (has_duplicates:true OR path:/missing)`,
	} {
		mixed := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"people":[%s]}}`, text, strings.Join(entries, ",")))
		got, err := s.MaterializeQuerySnapshot(WithVisiblePeopleOnly(ctx), SnapshotRequest{Query: mixed})
		require.NoError(t, err, text)
		require.Len(t, got.Rows, 1, text)
		require.Equal(t, version.NodeID, got.Rows[0].NodeID, text)
		require.Equal(t, []string{"speaker"}, got.Rows[0].MatchedRoles, text)
	}
}

// Exercise the compiler boundary with real role operands, including the
// population wrapper that is added after the predicate has been bound.
func TestRoleSearchNearCompilerBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := WithVisiblePeopleOnly(t.Context())
	version := seedDocumentPeopleEvent(t, s, "budget.txt", "b7", nil)
	person, err := s.CreatePerson(ctx, "Budget Sender", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)

	block := strings.TrimSuffix(strings.Repeat(`from:"Budget Sender" OR `, 100), " OR ")
	_, err = s.CreateSavedQuery(ctx, "Budget block", "", SavedQueryKindQuery, []byte(fmt.Sprintf(`{"syntax":"advanced","text":%q}`, block)))
	require.NoError(t, err)
	textFor := func(count int) string {
		return strings.TrimSuffix(strings.Repeat(`saved:"Budget block" OR `, count/100)+strings.Repeat(`from:"Budget Sender" OR `, count%100), " OR ")
	}
	var bestText string
	var bestSize, bestCount int
	low, high := 1, 1200
	for low <= high {
		count := (low + high) / 2
		text := textFor(count)
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, text))
		compiled, err := s.CompileQuery(ctx, value)
		if err != nil {
			require.ErrorContains(t, err, "compiled query exceeds")
			high = count - 1
			continue
		}
		predicate, err := compiled.bind("", nil)
		require.NoError(t, err)
		bestText, bestSize, bestCount = text, len(predicate.sql), count
		low = count + 1
	}
	require.Greater(t, bestSize, maxCompiledQuerySQL-8192)
	next := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, textFor(bestCount+1)))
	_, err = s.CompileQuery(ctx, next)
	require.ErrorContains(t, err, "compiled query exceeds")
	value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, bestText))
	result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, []string{"sender"}, result.Rows[0].MatchedRoles)
}

// A near-limit saved input must still execute when followed by relation leaves;
// the next original input must be rejected before membership lowering.
func TestRoleSearchNearCompilerBudgetWithRelations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := WithVisiblePeopleOnly(t.Context())
	version := seedDocumentPeopleEvent(t, s, "relation-budget.txt", "b71", nil)
	person, err := s.CreatePerson(ctx, "Relation Budget Sender", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	block := strings.TrimSuffix(strings.Repeat(`from:"Relation Budget Sender" OR `, 100), " OR ")
	_, err = s.CreateSavedQuery(ctx, "Relation budget block", "", SavedQueryKindQuery, []byte(fmt.Sprintf(`{"syntax":"advanced","text":%q}`, block)))
	require.NoError(t, err)
	collection, err := s.BeginIngest(ctx, "cli", "Synthetic budget collection")
	require.NoError(t, err)
	addCollectionMembership(t, s, collection, version.NodeID, "/synthetic/relation-budget.txt", nil)
	label := "Budget collection"
	_, err = s.SetCollectionLabel(ctx, collection.ID(), 1, &label)
	require.NoError(t, err)
	for _, relation := range []string{`path:/missing`, `collection:"Budget collection"`, `text_coverage:complete`, `has_duplicates:true`} {
		textFor := func(count int) string {
			base := strings.TrimSuffix(strings.Repeat(`saved:"Relation budget block" OR `, count/100)+strings.Repeat(`from:"Relation Budget Sender" OR `, count%100), " OR ")
			return "(" + base + ") AND (NOT " + relation + " OR name:relation-budget)"
		}
		low, high, best, size := 1, 1200, 0, 0
		for low <= high {
			count := (low + high) / 2
			value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, textFor(count)))
			compiled, err := s.CompileQuery(ctx, value)
			if err != nil {
				require.ErrorContains(t, err, "compiled query exceeds")
				high = count - 1
				continue
			}
			predicate, err := compiled.bind("", nil)
			require.NoError(t, err)
			best, size, low = count, len(predicate.sql), count+1
		}
		require.Greater(t, size, maxCompiledQuerySQL-8192, relation)
		next := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, textFor(best+1)))
		_, err = s.CompileQuery(ctx, next)
		require.ErrorContains(t, err, "compiled query exceeds", relation)
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, textFor(best)))
		result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Coverage: CoverageSelection{Configuration: "configured", ProfileFingerprint: testSHA256([]byte("budget-profile"))}})
		require.NoError(t, err, relation)
		require.Len(t, result.Rows, 1, relation)
		require.Equal(t, version.NodeID, result.Rows[0].NodeID, relation)
		require.Equal(t, []string{"sender"}, result.Rows[0].MatchedRoles, relation)
	}
}

func TestRoleSearchPeopleFacetMembershipBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "budget.txt", "aabb", nil)
	first, err := s.CreatePerson(ctx, "Budget First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(ctx, "Budget Second", "operator")
	require.NoError(t, err)
	for _, role := range []string{"author", "sender"} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: first.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	request := SnapshotRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"person_role"}}
	one, err := s.materializeQuerySnapshot(ctx, request, snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	require.True(t, one.Facets[0].Available)
	require.EqualValues(t, 1, *one.Facets[0].Total)
	require.ElementsMatch(t, []string{"author", "sender"}, one.Facets[0].Values[0].Roles)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: second.PersonID, Role: "recipient", Action: "assert", Revision: 1})
	require.NoError(t, err)
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
	first := seedDocumentPeopleEvent(t, s, "branch-one.txt", "ccdd", nil)
	second := seedDocumentPeopleEvent(t, s, "branch-two.txt", "ccdd", nil)
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
	for _, tc := range []struct {
		text  string
		roles []string
	}{
		{`from:"Branch Alpha"`, []string{"sender"}},
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

func TestRoleSearchSavedHiddenPersonScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hidden := browsePhotoNode(t, s, "saved-hidden.jpg", fakeHash("saved-hidden"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Hidden Depicted", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: hidden.CurrentVersionID, PersonID: person.PersonID, Role: "depicted", Action: "assert", Revision: 1})
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.NoError(t, s.LockPhotoHidden(ctx))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	definition, err := s.CreateSavedQuery(ctx, "Hidden saved", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"person:\"Hidden Depicted\""}`))
	require.NoError(t, err)
	structured, err := s.CreateSavedQuery(ctx, "Hidden structured", "", SavedQueryKindQuery, fmt.Appendf(nil, `{"filters":{"people":[{"person_id":%q,"roles":["depicted"]}]}}`, person.PersonID))
	require.NoError(t, err)
	nested, err := s.CreateSavedQuery(ctx, "Hidden nested", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Hidden structured\""}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	for _, tc := range []struct {
		name string
		ctx  context.Context
		ok   bool
	}{
		{"unlocked", WithVisiblePeopleOnly(WithPhotoHiddenToken(ctx, token)), false},
		{"locked", WithVisiblePeopleOnly(ctx), false},
		{"full", ctx, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, saved := range []SavedQuery{definition, structured, nested} {
				_, page, err := service.RunSaved(tc.ctx, "owner", saved.ID, saved.Revision, SnapshotRequest{}, nil)
				require.NoError(t, err)
				if tc.ok || saved.ID == definition.ID {
					require.Len(t, page.Rows, 1)
				} else {
					require.Empty(t, page.Rows)
				}
			}
		})
	}
}

func seedCurrentRoleSearchEvent(t *testing.T, s *Store, name, seed string, actors []document.DocumentEventActorV1) ContentVersion {
	t.Helper()
	version, target := ingestDocumentEventTarget(t, s, name, seed)
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,$1,$2,1,1,$3) ON CONFLICT(singleton) DO NOTHING`, document.DocumentEventsContractV1, DocumentEventsDeriverFingerprint, nowRFC3339())
	require.NoError(t, err)
	record := documentEventRecord(t, s.VaultID(), version.ID, seed)
	if actors != nil {
		record.Events[0].Actors = actors
	}
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
	require.NoError(t, err)
	return version
}

// Only root people entries are removed for the People facet. The expression
// and person_ids still fence the population used to count visible evidence.
func TestRoleSearchPeopleFacetRetainsExpressionAndPersonIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first := seedDocumentPeopleEvent(t, s, "keep-one.txt", "f101", nil)
	second := seedDocumentPeopleEvent(t, s, "keep-two.txt", "f102", nil)
	third := seedDocumentPeopleEvent(t, s, "outside.txt", "f103", nil)
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
	value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"name:keep AND NOT path:/missing","filters":{"person_ids":[%q],"people":[{"person_id":%q,"roles":["author"]}]}}`, anchor.PersonID, selected.PersonID))
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value, Facets: []string{"person_role"}})
	require.NoError(t, err)
	require.Empty(t, projection.Rows)
	facet := facetByDimension(t, projection, "person_role")
	require.EqualValues(t, 1, *facet.Total)
	require.Equal(t, map[string]int64{anchor.PersonID: 1, other.PersonID: 1, selected.PersonID: 0}, facetCounts(facet))
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
	filename, err := s.CreateFile(ctx, s.RootID(), "Linden Vale.txt", fakeHash("ce05"), 20, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, filename, "unrelated indexed body")
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `mentions:"Linden Vale"`, query.Filters{})
	require.NoError(t, s.withLexicalGenerationRead(ctx, func(q metadataQuerier, selected LexicalGeneration) error {
		require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, q, compiled, selected.ID))
		return nil
	}))
}

// Whole-AND annotation gates and self-excluded facets repeat person subqueries
// before relation leaves; each statement must use prepared leaf sets.
func TestRoleSearchPreparedLeafGatesAndFacets(t *testing.T) {
	t.Parallel()
	f := newPreparedLeafFixture(t)
	s, ctx := f.store, t.Context()
	leafCTEs := []string{"query_ancestry", "collection_members", "current_content_members", "processing_coverage"}
	for _, tc := range []struct {
		text string
		want int64
	}{
		{`author:"Leaf Author" AND has_duplicates:true AND path:/dir`, f.second},
		{`(author:"Leaf Author" OR author:"Leaf Absent") AND NOT has_duplicates:true`, f.unique},
		{`NOT (has_duplicates:true OR path:"/dir/second.txt") AND author:"Leaf Author"`, f.unique},
	} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, tc.text))
		result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.NoError(t, err, tc.text)
		require.Len(t, result.Rows, 1, tc.text)
		require.Equal(t, tc.want, result.Rows[0].NodeID, tc.text)
		require.Equal(t, []string{"author"}, result.Rows[0].MatchedRoles, tc.text)

		compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
		require.NoError(t, err, tc.text)
		withPopulationRead(t, s, func(q populationQuerier) {
			recording := &populationRecordingQuerier{populationQuerier: q}
			rows := []SnapshotRow{{NodeID: tc.want}}
			require.NoError(t, annotateMatchedRoles(ctx, recording, compiled, "", CoverageSelection{}, rows), tc.text)
			require.Equal(t, []string{"author"}, rows[0].MatchedRoles, tc.text)
			gates := 0
			for _, statement := range recording.statements {
				if !strings.HasPrefix(statement, "SELECT n.id FROM json_each(?) ids") || !strings.Contains(statement, "person_document_assertions") {
					continue
				}
				gates++
				for _, cte := range leafCTEs {
					require.NotContains(t, statement, cte, tc.text)
				}
			}
			require.Positive(t, gates, tc.text)
		})
	}

	withPopulationRead(t, s, func(q populationQuerier) {
		// The People facet drops only the people filter and keeps the leaf.
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"NOT author:\"Leaf Absent\" AND has_duplicates:false","filters":{"people":[{"person_id":%q,"roles":["author"]}]}}`, f.author))
		compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
		require.NoError(t, err)
		recording := &populationRecordingQuerier{populationQuerier: q}
		facet, err := materializePersonRoleFacet(ctx, recording, compiled, "", CoverageSelection{}, 1000)
		require.NoError(t, err)
		require.True(t, facet.Available)
		var author *SnapshotFacetValue
		for i := range facet.Values {
			if facet.Values[i].Key == f.author {
				author = &facet.Values[i]
			}
		}
		require.NotNil(t, author)
		require.Equal(t, int64(1), author.Count)
		require.Equal(t, []string{"author"}, author.Roles)
		facetSQL := recording.statements[len(recording.statements)-1]
		require.True(t, strings.HasPrefix(facetSQL, "WITH matched_members AS ("))
		require.Contains(t, facetSQL, populationMembersTable)
		for _, cte := range leafCTEs {
			require.NotContains(t, facetSQL, cte)
		}

		// A generic facet recompiles without its own filter and still prepares leaves.
		value = snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"author:\"Leaf Author\" AND NOT has_duplicates:true AND path:/dir","filters":{"collection_ids":[%q]}}`, f.collection))
		compiled, err = compileQuery(ctx, value, queryResolver{q: s.db})
		require.NoError(t, err)
		recording = &populationRecordingQuerier{populationQuerier: q}
		facet, err = materializeSnapshotFacet(ctx, recording, compiled, "", CoverageSelection{}, "collections", nil, 1000)
		require.NoError(t, err)
		require.True(t, facet.Available)
		prepared := false
		for _, statement := range recording.statements {
			if strings.HasPrefix(statement, "WITH matched_members AS (") {
				prepared = strings.Contains(statement, populationMembersTable) && !strings.Contains(statement, "current_content_members") && !strings.Contains(statement, "query_ancestry")
			}
		}
		require.True(t, prepared)
	})
}
