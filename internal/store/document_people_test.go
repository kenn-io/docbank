package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestDocumentPeopleGenerationPinsInputs(t *testing.T) {
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

func TestDocumentPeopleResolverInputsProvisionEligibleIdentityOnce(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "auto.txt", "d1", nil)

	first, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, first.Actors, 1)
	require.Len(t, first.Bindings["email:ada@example.test"], 1)
	require.Equal(t, "provisional", first.Bindings["email:ada@example.test"][0].State)
	require.Len(t, first.Persons, 1)

	second, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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

func TestDocumentPeopleDerivationDoesNotMutateAuditedAuthority(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "audited.txt", "d7", nil)
	audited, err := s.Mkdir(t.Context(), s.RootID(), "Audited")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, audited.ID)

	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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
		NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, DirtyRevision: input.DirtyRevision,
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
	s := newTestStore(t)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:ada lovelace", Claim: `"Ada Lovelace"`,
		DisplayName: "Ada Lovelace", Ordinal: 0, Role: "author"}
	secondActor := actor
	secondActor.Role = "last_saved_by"
	version := seedDocumentPeopleEvent(t, s, "name.txt", "d2", []document.DocumentEventActorV1{actor, secondActor})

	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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

	_, err = s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Equal(t, 1, candidates, "replay must reuse the evidence-keyed candidate")
}

func TestDocumentPeopleResolverInputsRejectsAuthorityBeyondAllocationBound(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "bounded.txt", "d6", nil)
	_, err := s.db.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<=?
	) INSERT INTO custodian_assignments(assignment_id,scope_kind,node_id,content_version_id,raw_label,raw_label_folded,rank,basis,source_ref,recorded_at)
	SELECT printf('assignment-%05d',value),'document',?,?,'Synthetic','synthetic','additional','operator_assigned','test',? FROM seq`,
		document.MaxPersonEdgesPerVersion+1, version.NodeID, version.ID, nowRFC3339())
	require.NoError(t, err)
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.ErrorIs(t, err, ErrPeopleInputsTooLarge)
	require.Empty(t, input.Custodians)
	var people int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM persons`).Scan(&people))
	require.Zero(t, people, "bounds are checked before identity provisioning")
}

func TestDocumentPeopleResolverInputsCountsCandidateQueueOverflow(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<?
	) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at)
	SELECT printf('00000000-0000-4000-8000-%012x',value),printf('name_alias:synthetic-%d',value),'Synthetic','name_only','[]',printf('%064x',value),1,1,'open',? FROM seq`,
		document.MaxOpenPersonCandidates, nowRFC3339())
	require.NoError(t, err)
	actor := document.DocumentEventActorV1{ActorKey: "name_alias:queue overflow", Claim: `"Queue Overflow"`,
		DisplayName: "Queue Overflow", Ordinal: 0, Role: "author"}
	version := seedDocumentPeopleEvent(t, s, "overflow.txt", "d4", []document.DocumentEventActorV1{actor})
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, input.CandidateOverflow)
}

func TestDocumentPeopleResolverInputsHonorsExternalUIDUnlink(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Synthetic transfer person", "transfer")
	require.NoError(t, err)
	identity := PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "person-1", UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: person.DisplayName}
	_, err = s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.NoError(t, err)
	key, err := document.ExternalPersonActorKey(identity.System, identity.ArchiveID, identity.UID)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "external.txt", "d5", nil)
	_, err = s.db.Exec(`DROP TRIGGER document_event_actors_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_event_actors SET actor_key=?,display_name=?,role='sender' WHERE generation_id=(SELECT generation_id FROM document_event_heads WHERE content_version_id=?)`,
		key, person.DisplayName, version.ID)
	require.NoError(t, err)
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings[key], 1)
	require.Equal(t, person.PersonID, input.Bindings[key][0].PersonID)
	require.NotEqual(t, "retired", input.Bindings[key][0].State)

	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.UnlinkExternalIdentity(t.Context(), identity.System, identity.ArchiveID, identity.UID, person.Revision))
	input, err = s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Bindings[key], 1)
	require.Equal(t, "retired", input.Bindings[key][0].State)
	var candidates int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_match_candidates`).Scan(&candidates))
	require.Zero(t, candidates, "an explicit unlink suppresses the transfer key instead of opening fallback review")
}

func TestPublishDocumentPeopleRejectsChangedInputsBeforeWriting(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, DocumentPeopleInputs)
	}{
		{"binding epoch", func(t *testing.T, s *Store, _ DocumentPeopleInputs) {
			t.Helper()
			_, err := s.db.Exec(`UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
			require.NoError(t, err)
		}},
		{"dirty revision", func(t *testing.T, s *Store, input DocumentPeopleInputs) {
			t.Helper()
			_, err := s.db.Exec(`UPDATE document_people_dirty SET revision=revision+1 WHERE content_version_id=?`, input.ContentVersionID)
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
			_, err := s.db.Exec(`INSERT INTO document_people_dirty(content_version_id,reason,marked_at,revision) VALUES(?,'test',?,1)`, version.ID, nowRFC3339())
			require.NoError(t, err)
			input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
			require.NoError(t, err)
			publication := documentPeoplePublicationForInput(t, input)
			test.mutate(t, s, input)

			_, err = s.PublishDocumentPeople(t.Context(), publication)
			require.ErrorIs(t, err, ErrPeopleInputsChanged)
			var generations, dirty int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_generations`).Scan(&generations))
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_dirty WHERE content_version_id=?`, version.ID).Scan(&dirty))
			require.Zero(t, generations)
			require.Equal(t, 1, dirty)
		})
	}
}

func TestPublishDocumentPeopleReplayInputIdentityAndRollup(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "publish.txt", "e1", nil)
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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
	var rollupDocuments int
	require.NoError(t, s.db.QueryRow(`SELECT document_count FROM person_rollups WHERE person_id=? AND disclosure_class='safe'`, edges[0].PersonID).Scan(&rollupDocuments))
	require.Equal(t, 1, rollupDocuments)
}

func TestPublishDocumentPeopleDetectsCollisionAndRollsBackForeignKeyFailure(t *testing.T) {
	t.Run("immutable collision", func(t *testing.T) {
		s := newTestStore(t)
		version := seedDocumentPeopleEvent(t, s, "collision.txt", "e2", nil)
		input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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
		input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "retry.txt", "f1", nil)
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
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
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "stale-failure.txt", "f2", nil)
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	var publicationEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&publicationEpoch))
	require.EqualValues(t, 2, publicationEpoch)
	_, err = s.db.Exec(`UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	newer, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), newer, "derivation_failed"))
	_, head, err := s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, documentPeopleStateFailed, head.State)
	require.Equal(t, "derivation_failed", head.FailureReason)
	require.Equal(t, newer.BindingEpoch, head.BindingEpoch)
	var edges, rollups int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, version.ID).Scan(&edges))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_rollups`).Scan(&rollups))
	require.Zero(t, edges)
	require.Zero(t, rollups)
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_people_state WHERE singleton=1`).Scan(&publicationEpoch))
	require.EqualValues(t, 3, publicationEpoch)
}

func seedDocumentPeopleEvent(t *testing.T, s *Store, name, seed string, actors []document.DocumentEventActorV1) ContentVersion {
	t.Helper()
	version, target := ingestDocumentEventTarget(t, s, name, seed)
	_, err := s.db.Exec(`INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,?,?,?,?,?)`,
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
		BindingEpoch: input.BindingEpoch, DirtyRevision: input.DirtyRevision,
	}
}

func fakeSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
