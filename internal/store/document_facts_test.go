package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func checkDocumentPeopleFactsMergeResolvesCandidate(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	first, err := s.CreatePerson(ctx, "First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(ctx, "Second", "operator")
	require.NoError(t, err)
	for _, p := range []Person{first, second} {
		_, err = s.AddPersonIdentity(ctx, p.PersonID, p.Revision, PersonIdentity{Kind: "email", ValueDisplay: "shared@example.test", Origin: "operator", EvidenceKind: "operator_assertion", EvidenceID: p.PersonID, Confidence: "operator_asserted"})
		require.NoError(t, err)
	}
	version, target := ingestDocumentEventTarget(t, s, "candidates.txt", "d2")
	record := documentEventRecord(t, s.VaultID(), version.ID, "d2")
	record.Events[0].Actors = []document.DocumentEventActorV1{
		{Role: "sender", ActorKey: "email:shared@example.test", Address: "shared@example.test", Claim: "shared@example.test"},
		{Role: "sender", Ordinal: 1, ActorKey: "name_alias:unknown", DisplayName: "Unknown", Claim: "Unknown"},
	}
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(ctx, DocumentPeoplePublication{
		People:     document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: version.ID, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}},
		Resolution: DocumentPeopleResolution{UnresolvedActors: 2}, InputsSHA256: fakeHash("a2"), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
	})
	require.NoError(t, err)
	facts, err := s.DocumentPeopleFacts(ctx, version.ID, false)
	require.NoError(t, err)
	require.Len(t, facts.Candidates, 3)
	first, _, err = s.PersonByID(ctx, first.PersonID)
	require.NoError(t, err)
	second, _, err = s.PersonByID(ctx, second.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, first.PersonID, second.PersonID, "00000000-0000-4000-8000-000000000011", first.Revision, second.Revision)
	require.NoError(t, err)
	input, err = s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	for _, actor := range input.Actors {
		if actor.ActorKey == "email:shared@example.test" {
			input.Actors = []DocumentPeopleActor{actor}
			break
		}
	}
	_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	for _, safe := range []bool{false, true} {
		facts, err = s.DocumentPeopleFacts(ctx, version.ID, safe)
		require.NoError(t, err)
		require.Len(t, facts.Edges, 1)
		require.Equal(t, first.PersonID, facts.Edges[0].PersonID)
		found := false
		for _, c := range facts.Candidates {
			switch c.ActorKey {
			case "email:shared@example.test":
				require.Empty(t, c.Occurrences)
			case "name_alias:unknown":
				require.Len(t, c.Occurrences, 1)
				require.Equal(t, "Unknown", c.Occurrences[0].Label)
				found = true
			}
		}
		require.True(t, found)
	}
}

func checkDocumentPeopleFactsAssertionsAndRetiredPerson(t *testing.T, newStore func(*testing.T) *Store) {
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
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.DocumentDateFacts(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
	record := coverageDocumentEventRecord(t, s.VaultID(), version.ID, "d4", "created", false)
	digest := requireDocumentEventInputsSHA256(t, s, target)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, digest, mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	facts, err = s.DocumentDateFacts(t.Context(), version.ID)
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

func checkDocumentPeopleFactsSensitiveUnresolvedActor(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:private author", Claim: `"Private Author"`, DisplayName: "Private Author", Role: "recipient", Sensitive: true}
	version, target := ingestDocumentEventTarget(t, s, "sensitive.txt", "d9")
	record := documentEventRecord(t, s.VaultID(), version.ID, "d9")
	record.Events[0].Sensitive = true
	record.Events[0].Actors = []document.DocumentEventActorV1{actor,
		{ActorKey: "name_alias:public author", Claim: "Public Author", DisplayName: "Public Author", Role: "author"},
		{ActorKey: "email:sender@example.test", Address: "sender@example.test", Claim: `"Private Name" <sender@example.test>`, DisplayName: "Private Name", Role: "blind_copy", Sensitive: true},
		{ActorKey: "email:sender@example.test", Address: "sender@example.test", Claim: `<sender@example.test>`, Role: "sender"}}
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
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
	var public *VersionCandidate
	for i := range safe.Candidates {
		if safe.Candidates[i].ActorKey == "name_alias:public author" {
			public = &safe.Candidates[i]
		}
	}
	require.NotNil(t, public)
	require.Len(t, public.Occurrences, 1)
	require.Equal(t, "Public Author", public.Occurrences[0].Label)
	foundSender := false
	for _, c := range safe.Candidates {
		if c.ActorKey == "email:sender@example.test" {
			require.Len(t, c.Occurrences, 1)
			require.Equal(t, "sender@example.test", c.Occurrences[0].Label)
			foundSender = true
		} else if c.ActorKey != "name_alias:public author" {
			require.Empty(t, c.Occurrences)
		}
	}
	require.True(t, foundSender)
	full, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Len(t, full.Candidates, 3)
	var sender *VersionCandidate
	for i := range full.Candidates {
		if full.Candidates[i].ActorKey == "email:sender@example.test" {
			sender = &full.Candidates[i]
		}
	}
	require.NotNil(t, sender)
	require.Len(t, sender.Occurrences, 2)
	require.Equal(t, "blind_copy", sender.Occurrences[0].Role)
	require.Equal(t, "Private Name", sender.Occurrences[0].Label)
	require.Equal(t, "sender", sender.Occurrences[1].Role)
	require.Equal(t, "sender@example.test", sender.Occurrences[1].Label)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	failed, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.State)
	require.Empty(t, failed.Candidates)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_people_heads SET resolver_fingerprint=$1 WHERE content_version_id=$2`, "old-resolver", version.ID)
	require.NoError(t, err)
	pending, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Equal(t, "pending", pending.State)
	require.Empty(t, pending.Candidates)
}

func checkDocumentPeopleFactsCandidateQueueOverflow(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	version, target := ingestDocumentEventTarget(t, s, "overflow.txt", "d4")
	fillFaceCandidateQueue(t, s, version.ID, maxOpenPersonCandidates)
	record := documentEventRecord(t, s.VaultID(), version.ID, "d4")
	record.Events[0].Actors = []document.DocumentEventActorV1{{ActorKey: "name_alias:queue overflow", Claim: "Queue Overflow", DisplayName: "Queue Overflow", Role: "author"}}
	_, err := s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, input.CandidateOverflow)
	_, err = s.PublishDocumentPeople(ctx, DocumentPeoplePublication{
		People:     document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: version.ID, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}},
		Resolution: DocumentPeopleResolution{UnresolvedActors: 1, CandidateOverflow: input.CandidateOverflow}, InputsSHA256: fakeHash("a3"), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
	})
	require.NoError(t, err)
	facts, err := s.DocumentPeopleFacts(ctx, version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "published", facts.State)
	require.Len(t, facts.Candidates, 1)
	require.Empty(t, facts.Candidates[0].CandidateID)
	require.Len(t, facts.Candidates[0].Occurrences, 1)
	require.Equal(t, "Queue Overflow", facts.Candidates[0].Occurrences[0].Label)
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
	_, err = s.DocumentPeopleFacts(ctx, photo.CurrentVersionID, true)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.DocumentPeopleFacts(WithPhotoHiddenToken(ctx, token), photo.CurrentVersionID, true)
	require.NoError(t, err)
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
