package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestDocumentPeopleGenerationPinsInputs(t *testing.T) {
	t.Parallel()
	p := DocumentPeoplePublication{
		People: document.DocumentPeopleV1{
			ContractVersion:  document.PersonContractV1,
			ContentVersionID: "00000000-0000-4000-8000-000000000001",
		},
		InputsSHA256:        strings.Repeat("a", 64),
		ResolverFingerprint: document.PersonResolverFingerprint(),
	}
	first, err := documentPeopleGenerationID(p, strings.Repeat("c", 64))
	require.NoError(t, err)
	p.InputsSHA256 = strings.Repeat("b", 64)
	next, err := documentPeopleGenerationID(p, strings.Repeat("c", 64))
	require.NoError(t, err)
	require.NotEqual(t, first, next)
}

func TestDocumentPeopleKeepsIndependentActorEvidenceAfterReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "people.db")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	version, target := ingestDocumentEventTarget(t, s, "undated.txt", "a1")
	_, err = s.db.Exec(`INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,?,?,?,?,?)`,
		document.DocumentEventsContractV1, fakeHash("e0"), 1, 1, nowRFC3339())
	require.NoError(t, err)
	record := documentEventRecord(t, s.VaultID(), version.ID, "a1")
	record.Events[0].Actors[0].EvidenceKind = "source_metadata"
	record.Events[0].Actors[0].EvidenceID = "undated-author"
	record.Sources = append(record.Sources, document.DocumentEventSourceV1{
		EvidenceKind: "source_metadata", EvidenceID: "undated-author", EvidenceSHA256: fakeHash("a2"),
	})
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(t.Context(), target, fakeHash("e0"), requireDocumentEventInputsSHA256(t, s, target), raw)
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Actors, 1)
	require.Equal(t, "undated-author", input.Actors[0].EvidenceID)
	require.Empty(t, input.Actors[0].AxisKey, "the event's date does not date independent actor evidence")
	want := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(t.Context(), want)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = Open(path)
	require.NoError(t, err)
	edges, head, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, want.People.Edges, edges)
	require.Equal(t, documentPeopleStatePublished, head.State)
}

func TestDocumentPeopleResolverInputsProvisionEligibleIdentityOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "auto.txt", "d1", nil)

	first, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, first.Actors, 1)
	require.Len(t, first.Bindings["email:ada@example.test"], 1)
	require.Equal(t, "provisional", first.Bindings["email:ada@example.test"][0].State)
	require.Len(t, first.Persons, 1)

	second, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, first.BindingEpoch, second.BindingEpoch)
	require.Equal(t, first.Bindings["email:ada@example.test"][0].PersonID,
		second.Bindings["email:ada@example.test"][0].PersonID)
	var people, identities int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM persons`).Scan(&people))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_identities`).Scan(&identities))
	require.Equal(t, 1, people)
	require.Equal(t, 1, identities)
}

func TestDocumentPeopleBoundsDerivedLabelsWithoutChangingEvidence(t *testing.T) {
	t.Parallel()
	longName := strings.Repeat("界", 67)
	longAddress := strings.Repeat("a", 192) + "@example.test"
	for _, tc := range []struct {
		name, key, display, address, want string
		candidate                         bool
	}{
		{name: "person name", key: "email:ada@example.test", display: longName, want: strings.Repeat("界", 66)},
		{name: "person address", key: "email:" + longAddress, address: longAddress, want: longAddress[:200]},
		{name: "candidate name", key: "name_alias:" + longName, display: longName, want: strings.Repeat("界", 66), candidate: true},
		{name: "candidate key", key: "name_alias:" + longName, want: strings.Repeat("界", 66), candidate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			version := seedDocumentPeopleEvent(t, s, "long-label.txt", "d7", []document.DocumentEventActorV1{{
				Role: "author", ActorKey: tc.key, DisplayName: tc.display, Address: tc.address, Claim: `"author"`,
			}})
			input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
			require.NoError(t, err)
			require.Len(t, input.Actors, 1)
			require.Equal(t, tc.key, input.Actors[0].ActorKey)
			require.Equal(t, tc.display, input.Actors[0].DisplayName)
			require.Equal(t, tc.address, input.Actors[0].Address)
			if tc.candidate {
				candidates, _, err := s.PersonCandidates(t.Context(), "open", 10, 0)
				require.NoError(t, err)
				require.Len(t, candidates, 1)
				require.Equal(t, tc.want, candidates[0].DisplayName)
				require.Equal(t, tc.key, candidates[0].ActorKey)
				return
			}
			require.Len(t, input.Bindings[tc.key], 1)
			person := input.Bindings[tc.key][0]
			require.Equal(t, tc.want, person.DisplayName)
			var identity string
			require.NoError(t, s.db.QueryRow(`SELECT kind || ':' || value_normalized FROM person_identities WHERE person_id=?`, person.PersonID).Scan(&identity))
			require.Equal(t, tc.key, identity)
		})
	}
}

func TestDocumentPeopleProvisioningKeepsPublishedVersionsCurrent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for index, address := range []string{"ada@example.test", "grace@example.test"} {
		version := seedDocumentPeopleEvent(t, s, address+".txt", fakeSHA256([]byte(address)), []document.DocumentEventActorV1{{
			Role: "sender", ActorKey: "email:" + address, Address: address, DisplayName: address, Claim: `"sender"`,
		}})
		input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
		require.NoError(t, err)
		targets, err := s.MissingDocumentPeopleTargetsAfter(t.Context(), document.PersonResolverFingerprint(), "", 100)
		require.NoError(t, err)
		require.Empty(t, targets, "provisioning another sender must not invalidate published versions")
		var generations int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_generations`).Scan(&generations))
		require.Equal(t, index+1, generations)
	}
}

func TestDocumentPeopleNameAliasIsOnlyASuggestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Ada", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "alias-a", Confidence: "name_candidate",
	})
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "alias.txt", "b1", []document.DocumentEventActorV1{{
		Role: "author", ActorKey: "name_alias:ada", DisplayName: "Ada", Claim: `"Ada"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Empty(t, input.Bindings["name_alias:ada"])
	var suggestion string
	require.NoError(t, s.db.QueryRow(`SELECT suggested_person_id FROM person_match_candidates WHERE reason='name_only'`).Scan(&suggestion))
	require.Equal(t, person.PersonID, suggestion)
}

func TestDocumentPeopleReusedIdentitySuggestsOnlyActiveOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	old, err := s.CreatePerson(ctx, "Previous Owner", "operator")
	require.NoError(t, err)
	identity := PersonIdentity{Kind: "email", ValueDisplay: "reused@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "reassigned-address", Confidence: "operator_asserted"}
	_, err = s.AddPersonIdentity(ctx, old.PersonID, old.Revision, identity)
	require.NoError(t, err)
	old, _, err = s.PersonByID(ctx, old.PersonID)
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, old.PersonID, old.Revision)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "reused.txt", "b3", []document.DocumentEventActorV1{{
		Role: "sender", ActorKey: "email:reused@example.test", Address: "reused@example.test", Claim: `"Sender"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings["email:reused@example.test"], 1)
	require.Equal(t, "retired", input.Bindings["email:reused@example.test"][0].State)
	candidates, _, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Empty(t, candidates)

	current, err := s.CreatePerson(ctx, "Current Owner", "operator")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, current.PersonID, current.Revision, identity)
	require.NoError(t, err)
	input, err = s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings["email:reused@example.test"], 2, "the reused identity must remain ambiguous")
	require.ElementsMatch(t, []string{old.PersonID, current.PersonID}, []string{
		input.Bindings["email:reused@example.test"][0].PersonID, input.Bindings["email:reused@example.test"][1].PersonID,
	})
	candidates, _, err = s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, current.PersonID, candidates[0].SuggestedPersonID)
}

func TestDocumentPeopleRetiredNameAliasKeepsUnassignedCandidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Former Author", "operator")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Former Author", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "retired-alias", Confidence: "name_candidate",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "retired-alias.txt", "b4", []document.DocumentEventActorV1{{
		Role: "author", ActorKey: "name_alias:former author", DisplayName: "Former Author", Claim: `"Former Author"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.Empty(t, input.Bindings["name_alias:former author"])
	candidates, _, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Empty(t, candidates[0].SuggestedPersonID)
	require.Equal(t, "name_only", candidates[0].Reason)
}

func TestDocumentPeopleDisplayOnlyActorRemainsUnresolved(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "display-only.txt", "b2", []document.DocumentEventActorV1{{
		Role: "author", DisplayName: "An unnamed contributor", Claim: `"contributor"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Actors, 1)
	require.Empty(t, input.Bindings)
	var candidates int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Zero(t, candidates)
}

func TestDocumentPeopleLoadFailureHasNoPublishableInputs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), "00000000-0000-4000-8000-000000000099")
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, input.ContentVersionID)
}

func TestDocumentPeopleDerivationDoesNotMutateAuditedAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "audited.txt", "d7", nil)
	audited, err := s.Mkdir(t.Context(), s.RootID(), "Audited")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, audited.ID)

	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Actors, 1)
	require.Empty(t, input.Bindings["email:ada@example.test"])

	raw, err := canonical.Marshal(input)
	require.NoError(t, err)
	publication := DocumentPeoplePublication{
		People: document.DocumentPeopleV1{
			ContractVersion:   document.PersonContractV1,
			ContentVersionID:  version.ID,
			EventGenerationID: input.EventGenerationID,
			Edges:             []document.DocumentPersonEdgeV1{},
		},
		Resolution:   DocumentPeopleResolution{UnresolvedActors: 1},
		InputsSHA256: fakeSHA256(raw), ResolverFingerprint: document.PersonResolverFingerprint(),
		NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
	}
	head, err := s.PublishDocumentPeople(t.Context(), publication)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStatePublished, head.State)
	require.Zero(t, head.EdgeCount)

	var people, identities, candidates int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM persons`).Scan(&people))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_identities`).Scan(&identities))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Zero(t, people)
	require.Zero(t, identities)
	require.Zero(t, candidates)
}

func TestDocumentPeopleResolverInputsOpensNameOnlyCandidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:ada lovelace", Claim: `"Ada Lovelace"`,
		DisplayName: "Ada Lovelace", Ordinal: 0, Role: "author"}
	secondActor := actor
	secondActor.Role = "last_saved_by"
	version := seedDocumentPeopleEvent(t, s, "name.txt", "d2", []document.DocumentEventActorV1{actor, secondActor})

	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Empty(t, input.Bindings[actor.ActorKey])
	var people, candidates int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM persons`).Scan(&people))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates WHERE state='open' AND reason='name_only'`).Scan(&candidates))
	require.Zero(t, people)
	require.Equal(t, 1, candidates)
	var evidence []byte
	require.NoError(t, s.db.QueryRow(`SELECT evidence_json FROM person_match_candidates`).Scan(&evidence))
	occurrences, err := decodeStoredCandidateEvidence(evidence)
	require.NoError(t, err)
	require.Len(t, occurrences, 2)

	_, err = s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Equal(t, 1, candidates, "replay must reuse the evidence-keyed candidate")
}

func TestDocumentPeopleResolverInputsRejectsAuthorityBeyondAllocationBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "bounded.txt", "d6", nil)
	_, err := s.db.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<=?
	) INSERT INTO custodian_assignments(assignment_id,scope_kind,node_id,content_version_id,raw_label,raw_label_folded,rank,basis,source_ref,recorded_at)
	SELECT printf('assignment-%05d',value),'document',?,?,'Synthetic','synthetic','additional','operator_assigned','test',? FROM seq`,
		document.MaxPersonEdgesPerVersion+1, version.NodeID, version.ID, nowRFC3339())
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.ErrorIs(t, err, ErrPeopleInputsTooLarge)
	require.Empty(t, input.Custodians)
	var people int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM persons`).Scan(&people))
	require.Zero(t, people, "bounds are checked before identity provisioning")
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "input_over_limit"))
	_, head, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStateUnavailable, head.State)
}

func TestDocumentPeopleResolverInputsCountsCandidateQueueOverflow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<?
	) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at)
	SELECT printf('00000000-0000-4000-8000-%012x',value),printf('name_alias:synthetic-%d',value),'Synthetic','name_only','[]',printf('%064x',value),1,1,'open',? FROM seq`,
		maxOpenPersonCandidates, nowRFC3339())
	require.NoError(t, err)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:queue overflow", Claim: `"Queue Overflow"`,
		DisplayName: "Queue Overflow", Ordinal: 0, Role: "author"}
	version := seedDocumentPeopleEvent(t, s, "overflow.txt", "d4", []document.DocumentEventActorV1{actor})
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, input.CandidateOverflow)
}

func TestDocumentPeopleResolverInputsHonorsExternalUIDUnlink(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Synthetic transfer person", "transfer")
	require.NoError(t, err)
	identity := PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "person-1", UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: person.DisplayName}
	_, err = s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.NoError(t, err)
	key, err := document.ExternalPersonActorKey(identity.System, identity.ArchiveID, identity.UID)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "external.txt", "d5", []document.DocumentEventActorV1{{
		Role: "sender", ActorKey: key, DisplayName: person.DisplayName, Claim: `"external sender"`,
	}})
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings[key], 1)
	require.Equal(t, person.PersonID, input.Bindings[key][0].PersonID)
	require.NotEqual(t, "retired", input.Bindings[key][0].State)

	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.UnlinkExternalIdentity(t.Context(), identity.System, identity.ArchiveID, identity.UID, person.Revision))
	input, err = s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings[key], 1)
	require.Equal(t, "retired", input.Bindings[key][0].State)
	var candidates int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Zero(t, candidates, "an explicit unlink suppresses the transfer key instead of opening fallback review")
}

func TestPublishDocumentPeopleRejectsChangedInputsBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, DocumentPeopleInputs)
	}{
		{"binding epoch", func(t *testing.T, s *Store, _ DocumentPeopleInputs) {
			t.Helper()
			_, err := s.db.Exec(`UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
			require.NoError(t, err)
		}},
		{"event generation", func(t *testing.T, s *Store, input DocumentPeopleInputs) {
			t.Helper()
			generation := fakeHash("replacement-event")
			_, err := s.db.Exec(`INSERT INTO document_event_generations(generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,document_kind,canonical_json,checksum,event_count,created_at)
				VALUES(?,?,'document-events/v1',?,?,'other','{}',?,0,?)`, generation, input.ContentVersionID,
				fakeHash("resolver"), fakeHash("inputs"), fakeHash("checksum"), nowRFC3339())
			require.NoError(t, err)
			_, err = s.db.Exec(`UPDATE document_event_heads SET generation_id=? WHERE content_version_id=?`, generation, input.ContentVersionID)
			require.NoError(t, err)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			version := seedDocumentPeopleEvent(t, s, "fence.txt", "d3", nil)
			input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
			require.NoError(t, err)
			publication := documentPeoplePublicationForInput(t, input)
			test.mutate(t, s, input)

			_, err = s.PublishDocumentPeople(t.Context(), publication)
			require.ErrorIs(t, err, ErrPeopleInputsChanged)
			var generations int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_generations`).Scan(&generations))
			require.Zero(t, generations)
		})
	}
}

func TestPublishDocumentPeopleReplayInputIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "publish.txt", "e1", nil)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)

	first, err := s.PublishDocumentPeople(t.Context(), publication)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStatePublished, first.State)
	require.EqualValues(t, 1, first.EdgeCount)
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.EqualValues(t, 2, epoch)

	second, err := s.PublishDocumentPeople(t.Context(), publication)
	require.NoError(t, err)
	require.Equal(t, first.GenerationID, second.GenerationID)
	require.Equal(t, first.PublishedAt, second.PublishedAt)
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.EqualValues(t, 2, epoch)

	publication.InputsSHA256 = fakeHash("ab")
	third, err := s.PublishDocumentPeople(t.Context(), publication)
	require.NoError(t, err)
	require.NotEqual(t, first.GenerationID, third.GenerationID)
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.EqualValues(t, 3, epoch)

	edges, head, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, third.GenerationID, head.GenerationID)
	require.Equal(t, publication.People.Edges, edges)
}

func TestPublishDocumentPeopleDetectsCollisionAndRollsBackForeignKeyFailure(t *testing.T) {
	t.Parallel()
	t.Run("immutable collision", func(t *testing.T) {
		s := newTestStore(t)
		version := seedDocumentPeopleEvent(t, s, "collision.txt", "e2", nil)
		input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
		require.NoError(t, err)
		publication := documentPeoplePublicationForInput(t, input)
		raw, checksum, err := document.MarshalDocumentPeopleV1(publication.People)
		require.NoError(t, err)
		generationID, err := documentPeopleGenerationID(publication, checksum)
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO document_people_generations(generation_id,content_version_id,inputs_sha256,resolver_fingerprint,canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`,
			generationID, version.ID, publication.InputsSHA256, publication.ResolverFingerprint, append(raw, '\n'), checksum, nowRFC3339())
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(t.Context(), publication)
		require.ErrorIs(t, err, ErrDocumentPeopleCorrupt)
		var heads int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_heads`).Scan(&heads))
		require.Zero(t, heads)
	})

	t.Run("foreign key rollback", func(t *testing.T) {
		s := newTestStore(t)
		version := seedDocumentPeopleEvent(t, s, "rollback.txt", "e3", nil)
		input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
		require.NoError(t, err)
		publication := documentPeoplePublicationForInput(t, input)
		publication.People.Edges[0].PersonID = "00000000-0000-4000-8000-000000000099"
		_, checksum, err := document.MarshalDocumentPeopleV1(publication.People)
		require.NoError(t, err)
		generationID, err := documentPeopleGenerationID(publication, checksum)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(t.Context(), publication)
		require.Error(t, err)
		var generations int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_generations WHERE generation_id=?`, generationID).Scan(&generations))
		require.Zero(t, generations)
	})
}

func TestDocumentPeopleFailureRemainsRetryableAndCannotEraseSuccess(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "retry.txt", "f1", nil)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	targets, err := s.MissingDocumentPeopleTargetsAfter(t.Context(), document.PersonResolverFingerprint(), "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	publication := documentPeoplePublicationForInput(t, input)
	head, err := s.PublishDocumentPeople(t.Context(), publication)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStatePublished, head.State)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	_, current, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStatePublished, current.State)
	require.Empty(t, current.FailureReason)
}

func TestDocumentPeopleFailureReplacesOlderSuccessfulCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "stale-failure.txt", "f2", nil)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	var publicationEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&publicationEpoch))
	require.EqualValues(t, 2, publicationEpoch)
	_, err = s.db.Exec(`UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	newer, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), newer, "derivation_failed"))
	_, head, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStateFailed, head.State)
	require.Equal(t, "derivation_failed", head.FailureReason)
	require.Equal(t, newer.BindingEpoch, head.BindingEpoch)
	var edges int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, version.ID).Scan(&edges))
	require.Zero(t, edges)
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&publicationEpoch))
	require.EqualValues(t, 3, publicationEpoch)
}

func TestDocumentPeopleReadsWithholdStaleAttributionAndRetainHistory(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"person merge", "event evidence"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			survivor, err := s.CreatePerson(ctx, "Synthetic survivor", "operator")
			require.NoError(t, err)
			version := seedDocumentPeopleEvent(t, s, "freshness.txt", "b1", nil)
			before, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
			require.NoError(t, err)
			publication := documentPeoplePublicationForInput(t, before)
			original, err := s.PublishDocumentPeople(ctx, publication)
			require.NoError(t, err)
			originalJSON, _, err := document.MarshalDocumentPeopleV1(publication.People)
			require.NoError(t, err)

			if change == "person merge" {
				absorbed := before.Bindings[before.Actors[0].ActorKey][0]
				_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID,
					"00000000-0000-4000-8000-000000000011", survivor.Revision, absorbed.Revision)
			} else {
				_, err = s.PublishSourceMetadata(ctx, version.BlobHash, fakeHash("c7"), mustSourceMetadata(t, "changed evidence"))
			}
			require.NoError(t, err)
			edges, _, err := s.DocumentPeopleForVersion(ctx, version.ID)
			require.ErrorIs(t, err, ErrNotFound)
			require.Empty(t, edges)

			if change == "event evidence" {
				targets, err := s.MissingDocumentEventTargetsAfter(ctx, fakeHash("e0"), "", 100)
				require.NoError(t, err)
				target := byTargetID(targets, version.ID)
				record := documentEventRecord(t, s.VaultID(), version.ID, "b2")
				_, err = s.PublishDocumentEvents(ctx, target, fakeHash("e0"),
					requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
				require.NoError(t, err)
				_, _, err = s.DocumentPeopleForVersion(ctx, version.ID)
				require.ErrorIs(t, err, ErrNotFound, "new events still require new person attribution")
			}
			after, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
			require.NoError(t, err)
			publication = documentPeoplePublicationForInput(t, after)
			_, err = s.PublishDocumentPeople(ctx, publication)
			require.NoError(t, err)
			edges, _, err = s.DocumentPeopleForVersion(ctx, version.ID)
			require.NoError(t, err)
			require.Equal(t, publication.People.Edges, edges)
			if change == "person merge" {
				require.Equal(t, survivor.PersonID, edges[0].PersonID)
			}
			var retainedJSON []byte
			require.NoError(t, s.db.QueryRow(`SELECT canonical_json FROM document_people_generations WHERE generation_id=?`, original.GenerationID).Scan(&retainedJSON))
			require.JSONEq(t, string(originalJSON), string(retainedJSON))
		})
	}
}

func TestDocumentPeopleMembershipChangesInvalidateOnlyAffectedDocument(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"trash", "restore", "replace"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			version := seedDocumentPeopleEvent(t, s, "membership.txt", "a1", nil)
			other := seedDocumentPeopleEvent(t, s, "unrelated.txt", "a2", nil)
			var ingestID string
			require.NoError(t, s.db.QueryRow(`SELECT ingest_id FROM provenance WHERE node_id=?`, version.NodeID).Scan(&ingestID))
			person, err := s.CreatePerson(ctx, "Synthetic Custodian", "operator")
			require.NoError(t, err)
			_, err = s.SetCustodian(ctx, CustodianRequest{
				Scope: CustodianScope{Kind: "collection", IngestID: ingestID}, PersonID: person.PersonID,
				RawLabel: "Synthetic Custodian", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1,
			})
			require.NoError(t, err)
			node, err := s.NodeByID(ctx, version.NodeID)
			require.NoError(t, err)
			if change == "restore" {
				node, _, err = s.Trash(ctx, node.ID, node.Revision)
				require.NoError(t, err)
			}
			before, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
			require.NoError(t, err)
			_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, before))
			require.NoError(t, err)
			unrelated, err := s.PrepareDocumentPeopleInputs(ctx, other.ID)
			require.NoError(t, err)
			_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, unrelated))
			require.NoError(t, err)

			switch change {
			case "trash":
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			case "restore":
				_, _, err = s.Restore(ctx, node.ID, node.Revision)
			case "replace":
				_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("a4"), 12, "text/plain")
			}
			require.NoError(t, err)
			edges, _, err := s.DocumentPeopleForVersion(ctx, version.ID)
			require.ErrorIs(t, err, ErrNotFound)
			require.Empty(t, edges)
			_, _, err = s.DocumentPeopleForVersion(ctx, other.ID)
			require.NoError(t, err, "unrelated attribution remains readable")
			after, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
			require.NoError(t, err)
			require.Equal(t, before.BindingEpoch, after.BindingEpoch)
			require.Equal(t, before.EventGenerationID, after.EventGenerationID)
			if change == "restore" {
				require.Empty(t, before.Custodians)
				require.Len(t, after.Custodians, 1)
			} else {
				require.Len(t, before.Custodians, 1)
				require.Empty(t, after.Custodians)
			}
			_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, before))
			require.ErrorIs(t, err, ErrPeopleInputsChanged)
			require.ErrorIs(t, s.MarkDocumentPeopleFailed(ctx, before, "derivation_failed"), ErrPeopleInputsChanged)
			targets, err := s.MissingDocumentPeopleTargetsAfter(ctx, document.PersonResolverFingerprint(), "", 100)
			require.NoError(t, err)
			require.Equal(t, []DocumentPeopleTarget{{ContentVersionID: version.ID, NodeID: version.NodeID, Reason: "stale"}}, targets)
			_, err = s.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, after))
			require.NoError(t, err)
			_, current, err := s.DocumentPeopleForVersion(ctx, version.ID)
			require.NoError(t, err)
			require.Equal(t, after.NodeRevision, current.NodeRevision)
			targets, err = s.MissingDocumentPeopleTargetsAfter(ctx, document.PersonResolverFingerprint(), "", 100)
			require.NoError(t, err)
			require.Empty(t, targets)
		})
	}
}

func seedDocumentPeopleEvent(t *testing.T, s *Store, name, seed string, actors []document.DocumentEventActorV1) ContentVersion {
	t.Helper()
	version, target := ingestDocumentEventTarget(t, s, name, seed)
	_, err := s.db.Exec(`INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,?,?,?,?,?) ON CONFLICT(singleton) DO NOTHING`,
		document.DocumentEventsContractV1, fakeHash("e0"), 1, 1, nowRFC3339())
	require.NoError(t, err)
	record := documentEventRecord(t, s.VaultID(), version.ID, seed)
	if actors != nil {
		record.Events[0].Actors = actors
	}
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	inputsSHA := requireDocumentEventInputsSHA256(t, s, target)
	_, err = s.PublishDocumentEvents(t.Context(), target, fakeHash("e0"), inputsSHA, raw)
	require.NoError(t, err)
	return version
}

func documentPeoplePublicationForInput(t *testing.T, input DocumentPeopleInputs) DocumentPeoplePublication {
	t.Helper()
	raw, err := canonical.Marshal(input)
	require.NoError(t, err)
	sum := fakeSHA256(raw)
	claim := input.Actors[0]
	person := input.Bindings[claim.ActorKey][0]
	return DocumentPeoplePublication{
		People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1,
			ContentVersionID: input.ContentVersionID, EventGenerationID: input.EventGenerationID,
			Edges: []document.DocumentPersonEdgeV1{{PersonID: person.PersonID, Role: claim.Role,
				ActorKey: claim.ActorKey, EvidenceKind: claim.EvidenceKind, EvidenceID: claim.EvidenceID,
				Confidence: "exact_identifier", Basis: "identifier_match", RawLabel: claim.DisplayName,
				ClaimCount: 1, FirstAxisKey: claim.AxisKey, LastAxisKey: claim.AxisKey}}},
		InputsSHA256: sum, ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID,
		BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
	}
}

func fakeSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
