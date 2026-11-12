package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentFactsDatesState(t *testing.T) {
	s := newTestStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "example.txt", "a1")
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.DocumentDateFacts(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDocumentPeopleFactsMissingHead(t *testing.T) {
	s := newTestStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "people.txt", "a2")
	facts, err := s.DocumentPeopleFacts(t.Context(), version.ID, true)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.Edges)
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
}

func checkDocumentPeopleFactsPublicationAndFailure(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	version := seedDocumentPeopleEvent(t, s, "people-published.txt", "c4", nil)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	facts, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Equal(t, "published", facts.State)
	require.NotEmpty(t, facts.Edges)
	failedVersion := seedDocumentPeopleEvent(t, s, "people-failed.txt", "c5", nil)
	failedInput, err := s.PrepareDocumentPeopleInputs(t.Context(), failedVersion.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), failedInput, "derivation_failed"))
	facts, err = s.DocumentPeopleFacts(t.Context(), failedVersion.ID, false)
	require.NoError(t, err)
	require.Equal(t, "failed", facts.State)
	require.Empty(t, facts.Edges)
}
