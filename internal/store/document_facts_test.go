package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func checkDocumentFactsDatesState(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "example.txt", "a1")
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.DocumentDateFacts(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}

func checkDocumentPeopleFactsFiltersBlindCopyAssertion(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	_, versionID := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Synthetic Bcc", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: versionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	safe, err := s.DocumentPeopleFacts(t.Context(), versionID, true)
	require.NoError(t, err)
	require.Empty(t, safe.Assertions)
	full, err := s.DocumentPeopleFacts(t.Context(), versionID, false)
	require.NoError(t, err)
	require.Len(t, full.Assertions, 1)
	visible, err := s.CreatePerson(t.Context(), "Retired Author", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: versionID, PersonID: visible.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.RetirePerson(t.Context(), visible.PersonID, visible.Revision)
	require.NoError(t, err)
	safe, err = s.DocumentPeopleFacts(t.Context(), versionID, true)
	require.NoError(t, err)
	require.Len(t, safe.Assertions, 1)
	require.Equal(t, "Retired Author", safe.Persons[visible.PersonID].Person.DisplayName)
}

func checkDocumentDateFactsNeverReturnsStaleClaims(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	version, target := ingestDocumentEventTarget(t, s, "date.txt", "d3")
	record := coverageDocumentEventRecord(t, s.VaultID(), version.ID, "d4", "created", false)
	digest := requireDocumentEventInputsSHA256(t, s, target)
	_, err := s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, digest, mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "published", facts.State)
	require.NotEmpty(t, facts.View.Events.Events)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO document_event_dirty(content_version_id,revision,reason) VALUES($1,1,'test') ON CONFLICT(content_version_id) DO UPDATE SET revision=document_event_dirty.revision+1`, version.ID)
	require.NoError(t, err)
	facts, err = s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_event_attempts SET input_revision=(SELECT revision FROM document_event_dirty WHERE content_version_id=$1),state='failed' WHERE content_version_id=$1`, version.ID)
	require.NoError(t, err)
	facts, err = s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_event_attempts SET state='unavailable' WHERE content_version_id=$1`, version.ID)
	require.NoError(t, err)
	facts, err = s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", facts.State)
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), fakeHash("ab")))
	facts, err = s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", facts.State)
}

func sensitiveUnresolvedActorFixture(t *testing.T, newStore func(*testing.T) *Store) (*Store, string, DocumentPeopleInputs, PersonDocumentAssertion) {
	t.Helper()
	s := newStore(t)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:private author", Claim: `"Private Author"`, DisplayName: "Private Author", Role: "recipient", Sensitive: true}
	version, target := ingestDocumentEventTarget(t, s, "sensitive.txt", "d9")
	record := documentEventRecord(t, s.VaultID(), version.ID, "d9")
	record.Events[0].Actors = []document.DocumentEventActorV1{actor,
		{ActorKey: "email:sender@example.test", Address: "sender@example.test", Claim: `"Private Name" <sender@example.test>`, DisplayName: "Private Name", Role: "blind_copy", Sensitive: true},
		{ActorKey: "email:sender@example.test", Address: "sender@example.test", Claim: `<sender@example.test>`, Role: "sender"}}
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
	require.NoError(t, err)
	person, err := s.CreatePerson(t.Context(), "Asserted Author", "operator")
	require.NoError(t, err)
	assertion, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	// Exact email actors are auto-provisioned, so seed the unresolved candidate
	// whose mixed-disclosure occurrences this test exercises.
	senderCandidate := candidateForTest(t, "email:sender@example.test", "Private Name", "", []PersonCandidateOccurrence{
		{ContentVersionID: version.ID, Role: "blind_copy", EvidenceKind: "source_metadata", EvidenceID: record.Events[0].EvidenceID},
		{ContentVersionID: version.ID, Role: "sender", EvidenceKind: "source_metadata", EvidenceID: record.Events[0].EvidenceID},
	})
	senderCandidate.Reason = "identifier_conflict"
	openCandidateForTest(t, s, senderCandidate)
	safe, err := s.DocumentPeopleFacts(t.Context(), version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "none", safe.State)
	foundSender := false
	for _, c := range safe.Candidates {
		if c.ActorKey == "email:sender@example.test" {
			require.Len(t, c.Occurrences, 1)
			require.Equal(t, "sender@example.test", c.Occurrences[0].Label)
			foundSender = true
		} else {
			require.Empty(t, c.Occurrences)
		}
	}
	require.True(t, foundSender)
	full, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Len(t, full.Candidates, 2)
	for _, c := range full.Candidates {
		if c.ActorKey == "email:sender@example.test" {
			for _, o := range c.Occurrences {
				if o.Role == "sender" {
					require.Equal(t, "sender@example.test", o.Label)
				}
			}
		}
	}
	return s, version.ID, input, assertion
}

func checkDocumentPeopleFactsPublicationAndFailure(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	version, _ := ingestDocumentEventTarget(t, s, "precedence.txt", "d1")
	facts, err := s.DocumentPeopleFacts(ctx, version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.Edges)
	person, err := s.CreatePerson(ctx, "Assigned Author", "operator")
	require.NoError(t, err)
	assertion, err := s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.StartDocumentEventRebuild(ctx, "00000000-0000-4000-8000-000000000003", DocumentEventRebuildRequestSHA256)
	require.NoError(t, err)
	facts, err = s.DocumentPeopleFacts(ctx, version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "pending", facts.State)
	require.Equal(t, []PersonDocumentAssertion{assertion}, facts.Assertions)
	target := requireDocumentEventTarget(t, s, version.ID)
	digest := requireDocumentEventInputsSHA256(t, s, target)
	require.NoError(t, s.RecordDocumentEventAttempt(ctx, target, digest, "failed", []byte(`[]`)))
	facts, err = s.DocumentPeopleFacts(ctx, version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "failed", facts.State)
	record := documentEventRecord(t, s.VaultID(), version.ID, "d1")
	raw := mustMarshalDocumentEvents(t, record)
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, digest, raw)
	require.NoError(t, err)
	publishPeople := func() {
		t.Helper()
		input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, input))
		require.NoError(t, err)
	}
	check := func(state string, edges bool) {
		t.Helper()
		facts, err := s.DocumentPeopleFacts(ctx, version.ID, false)
		require.NoError(t, err)
		require.Equal(t, state, facts.State)
		require.Equal(t, []PersonDocumentAssertion{assertion}, facts.Assertions)
		if edges {
			require.NotEmpty(t, facts.Edges)
		} else {
			require.Empty(t, facts.Edges)
		}
	}
	publishPeople()
	check("published", true)
	person, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Renamed")
	require.NoError(t, err)
	check("pending", false)
	_, err = s.StartDocumentEventRebuild(ctx, "00000000-0000-4000-8000-000000000001", DocumentEventRebuildRequestSHA256)
	require.NoError(t, err)
	check("pending", false)
	target = requireDocumentEventTarget(t, s, version.ID)
	digest = requireDocumentEventInputsSHA256(t, s, target)
	require.NoError(t, s.RecordDocumentEventAttempt(ctx, target, digest, "failed", []byte(`[]`)))
	check("failed", false)
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, digest, raw)
	require.NoError(t, err)
	publishPeople()
	check("published", true)
	_, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Renamed Again")
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(ctx, input, "derivation_failed"))
	check("failed", false)
	_, err = s.StartDocumentEventRebuild(ctx, "00000000-0000-4000-8000-000000000002", DocumentEventRebuildRequestSHA256)
	require.NoError(t, err)
	check("pending", false)
	target = requireDocumentEventTarget(t, s, version.ID)
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
	require.NoError(t, err)
	publishPeople()
	check("published", true)
}

func checkDocumentPeopleFactsSensitiveUnresolvedActor(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s, versionID, input, assertion := sensitiveUnresolvedActorFixture(t, newStore)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	checkSuppressedPeopleCandidates(t, s, versionID, assertion)
}

func checkSuppressedPeopleCandidates(t *testing.T, s *Store, versionID string, assertion PersonDocumentAssertion) {
	t.Helper()
	failed, err := s.DocumentPeopleFacts(t.Context(), versionID, false)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.State)
	require.Empty(t, failed.Candidates)
	require.Equal(t, []PersonDocumentAssertion{assertion}, failed.Assertions)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_people_heads SET resolver_fingerprint=$1 WHERE content_version_id=$2`, "old-resolver", versionID)
	require.NoError(t, err)
	pending, err := s.DocumentPeopleFacts(t.Context(), versionID, false)
	require.NoError(t, err)
	require.Equal(t, "pending", pending.State)
	require.Empty(t, pending.Candidates)
	require.Equal(t, []PersonDocumentAssertion{assertion}, pending.Assertions)
}

func checkDocumentPeopleFactsSafeHiddenVisibilityUsesCallerSession(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	photo := browsePhotoNode(t, s, "hidden-facts.jpg", fakeHash("c2"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Hidden Person", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(WithPhotoHiddenToken(ctx, token), photo.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE persons SET state='provisional' WHERE person_id=$1`, person.PersonID)
	require.NoError(t, err)
	_, err = s.DocumentPeopleFacts(ctx, photo.CurrentVersionID, true)
	require.ErrorIs(t, err, ErrNotFound)
	facts, err := s.DocumentPeopleFacts(WithVisiblePeopleOnly(WithPhotoHiddenToken(ctx, token)), photo.CurrentVersionID, true)
	require.NoError(t, err)
	require.True(t, facts.Persons[person.PersonID].Visible)
}

func checkDocumentPeopleFactsFiltersBlindCopyEdge(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	for _, tc := range []struct {
		name, role string
		sensitive  bool
	}{{"blind copy", "blind_copy", false}, {"sensitive author", "author", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			version, target := ingestDocumentEventTarget(t, s, "bcc.eml", "e4")
			record := documentEventRecord(t, s.VaultID(), version.ID, "e4")
			record.Events[0].Actors[0].Role = document.EventRole(tc.role)
			record.Events[0].Actors[0].Sensitive = tc.sensitive
			_, err := s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
			require.NoError(t, err)
			input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
			require.NoError(t, err)
			publication := documentPeoplePublicationForInput(t, input)
			publication.People.Edges[0].Sensitive = tc.sensitive
			_, err = s.PublishDocumentPeople(t.Context(), publication)
			require.NoError(t, err)
			full, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
			require.NoError(t, err)
			require.Len(t, full.Edges, 1)
			require.Equal(t, tc.role, full.Edges[0].Role)
			require.Equal(t, tc.sensitive, full.Edges[0].Sensitive)
			safe, err := s.DocumentPeopleFacts(t.Context(), version.ID, true)
			require.NoError(t, err)
			require.Empty(t, safe.Edges)
		})
	}
}
