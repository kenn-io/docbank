package store

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPeopleAffectedPersonsAreCapturedBeforeCascade(t *testing.T) {
	s := newTestStore(t)
	node, version := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Records Team", "operator")
	require.NoError(t, err)
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,
			role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive)
			VALUES(?,?,?,'generation-a','author','','operator_assertion','assertion-a','operator_asserted','operator_assigned','',1,0)`, version, node, person.PersonID)
		if err != nil {
			return err
		}
		ids, err := peoplePersonsForVersion(t.Context(), tx, version)
		require.NoError(t, err)
		require.Equal(t, []string{person.PersonID}, ids)
		// Exercise the same FK cascade as deleting a pruned or purged source node.
		_, err = tx.ExecContext(t.Context(), `DELETE FROM nodes WHERE id=?`, node)
		if err != nil {
			return err
		}
		after, err := peoplePersonsForVersion(t.Context(), tx, version)
		require.NoError(t, err)
		require.Empty(t, after)
		return nil
	}))
	read, _, err := s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Equal(t, person.PersonID, read.PersonID)
}

func TestDocumentPeopleInputsBoundRetainedActorLabels(t *testing.T) {
	s := newTestStore(t)
	actor := document.DocumentEventActorV1{
		ActorKey: "email:ada.lovelace@example.test", Address: "ada.lovelace@example.test",
		Claim: `{"address":"ada.lovelace@example.test"}`, DisplayName: strings.Repeat("Ada Lovelace ", 24),
		Role: "author",
	}
	version := seedDocumentPeopleEvent(t, s, "long-actor.txt", "a5", []document.DocumentEventActorV1{actor})
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, input.Actors, 1)
	require.LessOrEqual(t, len(input.Actors[0].DisplayName), document.MaxPersonDisplayNameBytes)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err, "a retained valid event actor must not poison people publication")
}

func TestAutoProvisionDoesNotInvalidateUnrelatedPeopleHeads(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "new-correspondent.txt", "a6", nil)
	var before int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&before))
	input, err := s.DocumentPeopleResolverInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, before, input.BindingEpoch)
	var after int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&after))
	require.Equal(t, before, after, "provisioning for one target must not stale the complete corpus")
}

func TestPrunedPersonCandidateEvidenceRemainsExportable(t *testing.T) {
	s := newTestStore(t)
	nodeID, versionID := seedPeopleVersion(t, s)
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:pruned-export", "Pruned Export", "", []PersonCandidateOccurrence{{
		ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "pruned-export-claim",
	}}))
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, nodeID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported),
		"candidate decisions intentionally outlive the evidence version")
}

func TestPersonRollupsTrackTrashRestoreAndCurrentHead(t *testing.T) {
	s := newTestStore(t)
	node, version, person := seedLifecyclePersonProjection(t, s)
	require.Equal(t, 1, personRollupDocumentCount(t, s, person.PersonID))

	trashed, _, err := s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	require.Zero(t, personRollupDocumentCount(t, s, person.PersonID))
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, coverage.Pending, "coverage includes retained files in trash")

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, 1, personRollupDocumentCount(t, s, person.PersonID))

	var updated Node
	var current ContentVersion
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		updated, current, err = installContentVersionTx(t.Context(), tx, restored,
			strings.Repeat("a", 64), 1, "text/plain", "content_replace", nil)
		return err
	}))
	require.NotEqual(t, version, current.ID)
	require.Equal(t, current.ID, updated.CurrentVersionID)
	require.Zero(t, personRollupDocumentCount(t, s, person.PersonID))
}

func TestPersonRollupRefreshesAfterPruneCascade(t *testing.T) {
	s := newTestStore(t)
	node, historical, person := seedLifecyclePersonProjection(t, s)
	current, err := newUUIDv4()
	require.NoError(t, err)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind)
			VALUES(?,?,?,1,'text/plain',?,2,?,'content_replace')`, current, node.ID, strings.Repeat("a", 64), nowRFC3339(), operation); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET current_version_id=?,revision=2 WHERE id=?`, current, node.ID)
		return err
	}))
	require.Equal(t, 1, personRollupDocumentCount(t, s, person.PersonID), "fixture starts with a stale rollup")

	receipt, err := s.PruneContentVersions(t.Context(), node.ID, 2,
		VersionPruneSelector{VersionIDs: []string{historical}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, receipt.DeletedVersions)
	require.Zero(t, personRollupDocumentCount(t, s, person.PersonID))
	_, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err, "pruning derived edges never collects person authority")
}

func TestEmailDerivativePurgeInvalidatesPeopleAndPreservesManualAuthority(t *testing.T) {
	s := newTestStore(t)
	email := newEmailFixture(t, s, "synthetic-message.eml")
	published, err := s.PublishEmailGeneration(t.Context(), email.publication)
	require.NoError(t, err)
	person, err := s.CreatePerson(t.Context(), "Ada Lovelace", "operator")
	require.NoError(t, err)
	assertion, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: published.Version.ID, PersonID: person.PersonID,
		Role: "author", Action: "assert", Note: "Synthetic operator assignment", Revision: 1,
	})
	require.NoError(t, err)
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,
			role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive)
			VALUES(?,?,?,'generation-email','sender','email:ada.lovelace@example.test','email_generation',?,'exact_identifier','identifier_match','Ada Lovelace',1,0)`,
			published.Version.ID, published.Version.NodeID, person.PersonID, published.Generation.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO document_people_heads(content_version_id,event_generation_id,inputs_sha256,generation_id,resolver_fingerprint,binding_epoch,edge_count,state,failure_reason,published_at)
			VALUES(?,'event-generation',?,'generation-email',?,1,1,'published','',?)`, published.Version.ID, fakeHash("d1"), fakeHash("d2"), nowRFC3339()); err != nil {
			return err
		}
		return s.RefreshPersonRollups(t.Context(), tx, []string{person.PersonID})
	}))
	require.Equal(t, 1, personRollupDocumentCount(t, s, person.PersonID))

	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{published.Version.ID}})
	require.NoError(t, err)
	require.Zero(t, personRollupDocumentCount(t, s, person.PersonID))
	var projected, heads, dirty, assertions int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, published.Version.ID).Scan(&projected))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_heads WHERE content_version_id=?`, published.Version.ID).Scan(&heads))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_dirty WHERE content_version_id=?`, published.Version.ID).Scan(&dirty))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_document_assertions WHERE assertion_id=?`, assertion.AssertionID).Scan(&assertions))
	require.Zero(t, projected)
	require.Zero(t, heads)
	require.Equal(t, 1, dirty)
	require.Equal(t, 1, assertions, "derivative purge retains explicit person authority")
}

func seedLifecyclePersonProjection(t *testing.T, s *Store) (Node, string, Person) {
	t.Helper()
	nodeID, version := seedPeopleVersion(t, s)
	node, err := s.NodeByID(t.Context(), nodeID)
	require.NoError(t, err)
	person, err := s.CreatePerson(t.Context(), "Ada Lovelace", "operator")
	require.NoError(t, err)
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,
			role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive)
			VALUES(?,?,?,'generation-lifecycle','author','','operator_assertion','assertion-lifecycle','operator_asserted','operator_assigned','',1,0)`, version, node.ID, person.PersonID); err != nil {
			return err
		}
		return s.RefreshPersonRollups(t.Context(), tx, []string{person.PersonID})
	}))
	return node, version, person
}

func personRollupDocumentCount(t *testing.T, s *Store, personID string) int {
	t.Helper()
	var count int
	err := s.db.QueryRow(`SELECT document_count FROM person_rollups WHERE person_id=? AND disclosure_class='safe'`, personID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return count
}
