package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentPeopleCoverageTracksRetainedLifecycle(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "lifecycle.txt", "a5", nil)
	person := publishLifecycleDocumentPeople(t, s, version.ID)
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, coverage.Published)

	trashed, _, err := s.Trash(t.Context(), version.NodeID, UnconditionalRev)
	require.NoError(t, err)
	coverage, err = s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Zero(t, coverage.Published)
	require.EqualValues(t, 1, coverage.Pending, "coverage includes retained files in trash")
	_, _, err = s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.ErrorIs(t, err, ErrNotFound, "the old publication cannot bypass the node revision fence")

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	publishLifecycleDocumentPeople(t, s, version.ID)
	coverage, err = s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, coverage.Published)

	updated, _, err := s.ReplaceContent(t.Context(), restored.ID, restored.Revision, fakeHash("a8"), 1, "text/plain")
	require.NoError(t, err)
	coverage, err = s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Zero(t, coverage.Published)
	require.EqualValues(t, 1, coverage.Pending, "the old version does not count as current coverage")

	receipt, err := s.PruneContentVersions(t.Context(), updated.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{version.ID}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, receipt.DeletedVersions)
	var edges int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, version.ID).Scan(&edges))
	require.Zero(t, edges)
	_, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err, "pruning derived links retains person authority")

	_, _, err = s.Trash(t.Context(), receipt.Node.ID, receipt.Node.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	coverage, err = s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Zero(t, coverage.Published+coverage.Pending+coverage.Failed+coverage.Unavailable)
}

func TestChangedSourceEvidenceRemovesDocumentPeople(t *testing.T) {
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "changed.txt", "a6", nil)
	person := publishLifecycleDocumentPeople(t, s, version.ID)
	before, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)

	_, err = s.PublishSourceMetadata(t.Context(), version.BlobHash, fakeHash("a7"), mustSourceMetadata(t, "changed"))
	require.NoError(t, err)
	_, _, err = s.DocumentPeopleForVersion(t.Context(), version.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var edges int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, version.ID).Scan(&edges))
	require.Zero(t, edges)
	after, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Greater(t, after.PublicationEpoch, before.PublicationEpoch)
	require.Zero(t, after.Published)
	require.EqualValues(t, 1, after.Pending)
	_, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
}

func TestEmailDerivativePurgeInvalidatesPeopleAndPreservesManualAuthority(t *testing.T) {
	s := newTestStore(t)
	email := newEmailFixture(t, s, "synthetic-message.eml")
	published, err := s.PublishEmailGeneration(t.Context(), email.publication)
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, published.Version.ID)
	record := documentEventRecord(t, s.VaultID(), published.Version.ID, "a9")
	record.DocumentKind = "email"
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	person := publishLifecycleDocumentPeople(t, s, published.Version.ID)
	assertion, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: published.Version.ID, PersonID: person.PersonID,
		Role: "author", Action: "assert", Note: "Synthetic operator assignment", Revision: 1,
	})
	require.NoError(t, err)
	publishLifecycleDocumentPeople(t, s, published.Version.ID)

	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{published.Version.ID}})
	require.NoError(t, err)
	_, _, err = s.DocumentPeopleForVersion(t.Context(), published.Version.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var projected, heads, dirty, assertions int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people WHERE content_version_id=?`, published.Version.ID).Scan(&projected))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_people_heads WHERE content_version_id=?`, published.Version.ID).Scan(&heads))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_dirty WHERE content_version_id=?`, published.Version.ID).Scan(&dirty))
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM person_document_assertions WHERE assertion_id=?`, assertion.AssertionID).Scan(&assertions))
	require.Zero(t, projected)
	require.Zero(t, heads)
	require.Equal(t, 1, dirty)
	require.Equal(t, 1, assertions, "derivative purge retains explicit person authority")
	_, err = s.PublishEmailGeneration(t.Context(), email.publication)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
}

func publishLifecycleDocumentPeople(t *testing.T, s *Store, versionID string) Person {
	t.Helper()
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), versionID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	edges, _, err := s.DocumentPeopleForVersion(t.Context(), versionID)
	require.NoError(t, err)
	require.NotEmpty(t, edges)
	return input.Bindings[input.Actors[0].ActorKey][0]
}
