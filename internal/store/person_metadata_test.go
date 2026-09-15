package store

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPersonMetadataRoundTrip(t *testing.T) {
	source := newTestStore(t)
	person, err := source.CreatePerson(t.Context(), "Ada Lovelace", "operator")
	require.NoError(t, err)
	identity, err := source.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "identity-1", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person, _, err = source.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.NoError(t, source.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "person-current", []string{"person-retired"}))
	_, err = source.LinkExternalIdentity(t.Context(), PersonExternalIdentity{
		PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic", UID: "person-current",
		UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: person.DisplayName,
	}, person.Revision)
	require.NoError(t, err)
	person, _, err = source.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, source, "metadata.txt", "a1", nil)
	_, err = source.SetCustodian(t.Context(), CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: version.NodeID, ContentVersionID: version.ID},
		PersonID: person.PersonID, RawLabel: person.DisplayName, Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = source.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: version.ID,
		PersonID: person.PersonID, Role: "author", Action: "assert", Note: "synthetic", Revision: 1})
	require.NoError(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: version.ID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim-1"}})
	require.NoError(t, err)
	require.NoError(t, source.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		_, _, err := source.OpenPersonCandidate(t.Context(), tx, PersonMatchCandidate{ActorKey: "name_alias:ada lovelace",
			DisplayName: person.DisplayName, SuggestedPersonID: person.PersonID, Reason: "name_only", Evidence: evidence})
		return err
	}))
	absorbed, err := source.CreatePerson(t.Context(), "Grace Hopper", "operator")
	require.NoError(t, err)
	mergeOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = source.MergePersons(t.Context(), person.PersonID, absorbed.PersonID, mergeOperation, person.Revision, absorbed.Revision)
	require.NoError(t, err)
	person, _, err = source.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	splitOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = source.SplitPerson(t.Context(), PersonSplitRequest{PersonID: person.PersonID, OperationID: splitOperation,
		DisplayName: "Ada Byron", Revision: person.Revision, IdentityIDs: []string{identity.IdentityID}})
	require.NoError(t, err)
	person, _, err = source.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	rebuildOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = source.RebuildDocumentPeople(t.Context(), rebuildOperation)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &data))
	for _, recordType := range []string{"person", "person_identity", "person_external_identity", "person_external_uid_alias",
		"person_alias", "person_merge", "person_split", "custodian_assignment", "person_document_assertion", "person_match_candidate"} {
		require.Contains(t, data.String(), `"type":"`+recordType+`"`)
	}
	require.Equal(t, 1, strings.Count(data.String(), `"type":"person_external_identity"`))
	require.NotContains(t, data.String(), rebuildOperation)
	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var backupData bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &backupData))
	require.NoError(t, snapshot.Close())
	for _, recordType := range []string{"person", "person_identity", "person_external_identity", "person_external_uid_alias",
		"person_alias", "person_merge", "person_split", "custodian_assignment", "person_document_assertion", "person_match_candidate"} {
		require.Contains(t, backupData.String(), `"type":"`+recordType+`"`)
	}
	var repeated bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &repeated))
	require.Equal(t, data.Bytes(), repeated.Bytes())
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(data.Bytes())))
	restored, _, err := target.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Equal(t, person, restored)
	var restoredData bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &restoredData))
	require.Equal(t, data.Bytes(), restoredData.Bytes())
	require.ErrorContains(t, target.ImportMetadata(t.Context(), bytes.NewReader(data.Bytes())), "not pristine")
}

func TestPersonMetadataRejectsDuplicateCorruptAndMismatchedAuthority(t *testing.T) {
	source := newTestStore(t)
	person, err := source.CreatePerson(t.Context(), "Synthetic Person", "operator")
	require.NoError(t, err)
	version, _ := ingestDocumentEventTarget(t, source, "authority.txt", "b1")
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	duplicate := metadataPerson{Type: metadataPersonType, PersonID: person.PersonID,
		DisplayName: person.DisplayName, DisplayNameFolded: person.DisplayNameFolded, Origin: person.Origin,
		State: person.State, Revision: person.Revision, CreatedAt: person.CreatedAt, UpdatedAt: person.UpdatedAt}
	corruptID, err := newUUIDv4()
	require.NoError(t, err)
	corrupt := duplicate
	corrupt.PersonID, corrupt.DisplayNameFolded = corruptID, "wrong fold"
	wrongNode := version.NodeID + 1000
	personID := person.PersonID
	wrongScope := metadataCustodianAssignment{Type: metadataCustodianAssignmentType,
		AssignmentID: "80000000-0000-4000-8000-000000000001", ScopeKind: "document",
		NodeID: &wrongNode, ContentVersionID: &version.ID, PersonID: &personID,
		RawLabel: person.DisplayName, RawLabelFolded: person.DisplayNameFolded,
		Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic", Revision: 1,
		RecordedAt: person.UpdatedAt}

	for name, input := range map[string][]byte{
		"duplicate":   appendMetadataRecords(t, exported.Bytes(), duplicate),
		"corrupt":     appendMetadataRecords(t, exported.Bytes(), corrupt),
		"wrong scope": appendMetadataRecords(t, exported.Bytes(), wrongScope),
	} {
		t.Run(name, func(t *testing.T) {
			target := newTestStore(t)
			require.Error(t, target.ImportMetadata(t.Context(), bytes.NewReader(input)))
		})
	}
}

func TestPersonMetadataRejectsInvalidCandidateAndAliasBounds(t *testing.T) {
	t.Run("superseded candidate decision", func(t *testing.T) {
		source := newTestStore(t)
		person, err := source.CreatePerson(t.Context(), "Synthetic Person", "operator")
		require.NoError(t, err)
		version, _ := ingestDocumentEventTarget(t, source, "candidate.txt", "c1")
		occurrences := []PersonCandidateOccurrence{{ContentVersionID: version.ID, Role: "author",
			EvidenceKind: "source_metadata", EvidenceID: "claim-1"}}
		evidence, err := canonical.Marshal(occurrences)
		require.NoError(t, err)
		digest, err := candidateEvidenceDigest(occurrences)
		require.NoError(t, err)
		candidate := metadataPersonMatchCandidate{
			Type: metadataPersonCandidateType, CandidateID: "80000000-0000-4000-8000-000000000001",
			ActorKey: "name_alias:synthetic person", DisplayName: person.DisplayName, Reason: "name_only",
			EvidenceJSON: evidence, EvidenceSHA256: digest, OccurrenceCount: 1, Revision: 1,
			State: "superseded", DecidedPersonID: &person.PersonID, CreatedAt: person.CreatedAt,
		}
		var exported bytes.Buffer
		require.NoError(t, source.ExportMetadata(t.Context(), &exported))
		target := newTestStore(t)
		err = target.ImportMetadata(t.Context(), bytes.NewReader(appendMetadataRecords(t, exported.Bytes(), candidate)))
		require.ErrorContains(t, err, "candidate decision state")
	})

	t.Run("open candidate queue", func(t *testing.T) {
		s := newTestStore(t)
		version, _ := ingestDocumentEventTarget(t, s, "queue.txt", "c2")
		occurrences := []PersonCandidateOccurrence{{ContentVersionID: version.ID, Role: "author",
			EvidenceKind: "source_metadata", EvidenceID: "claim-2"}}
		evidence, err := canonical.Marshal(occurrences)
		require.NoError(t, err)
		digest, err := candidateEvidenceDigest(occurrences)
		require.NoError(t, err)
		_, err = s.db.Exec(`WITH RECURSIVE seq(value) AS (
			VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<?
		) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at)
		SELECT printf('80000000-0000-4000-8000-%012x',value),printf('name_alias:synthetic-%d',value),'Synthetic','name_only',?,?,1,1,'open',? FROM seq`,
			document.MaxOpenPersonCandidates+1, evidence, digest, nowRFC3339())
		require.NoError(t, err)
		require.ErrorContains(t, validatePersonMetadataState(t.Context(), s.db), "candidate queue")
	})

	t.Run("external UID alias hop limit", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.db.Exec(`WITH RECURSIVE seq(value) AS (
			VALUES(0) UNION ALL SELECT value+1 FROM seq WHERE value<32
		) INSERT INTO person_external_uid_aliases(system,archive_id,retired_uid,surviving_uid,observed_at)
		SELECT 'msgvault','synthetic',printf('uid-%02d',value),printf('uid-%02d',value+1),? FROM seq`, nowRFC3339())
		require.NoError(t, err)
		require.ErrorContains(t, validatePersonMetadataState(t.Context(), s.db), "hop limit")
	})
}
