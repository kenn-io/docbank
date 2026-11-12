package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDocumentFactsDatesState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "example.txt", "a1")
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.View.Events.Events)
	other, _ := ingestDocumentEventTarget(t, s, "other.txt", "a3")
	otherFacts, err := s.DocumentDateFacts(t.Context(), other.ID)
	require.NoError(t, err)
	require.Equal(t, other.ID, otherFacts.Version.ID)
	require.Equal(t, "none", otherFacts.State)
	_, err = s.DocumentDateFacts(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDocumentPeopleFactsMissingHead(t *testing.T) {
	t.Parallel()
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
}

func checkDocumentPeopleFactsPublicationAndFailure(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	version, target := ingestDocumentEventTarget(t, s, "people-published.txt", "c4")
	record := documentEventRecord(t, s.VaultID(), version.ID, "c4")
	raw := mustMarshalDocumentEvents(t, record)
	_, err := s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), raw)
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	facts, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Equal(t, "published", facts.State)
	require.NotEmpty(t, facts.Edges)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_people_heads SET resolver_fingerprint=$1 WHERE content_version_id=$2`, "old-resolver", version.ID)
	require.NoError(t, err)
	facts, err = s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Equal(t, "pending", facts.State)
	require.Empty(t, facts.Edges)
	s = newStore(t)
	failedVersion := seedDocumentPeopleEvent(t, s, "people-failed.txt", "c5", nil)
	failedInput, err := s.PrepareDocumentPeopleInputs(t.Context(), failedVersion.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), failedInput, "derivation_failed"))
	facts, err = s.DocumentPeopleFacts(t.Context(), failedVersion.ID, false)
	require.NoError(t, err)
	require.Equal(t, "failed", facts.State)
	require.Empty(t, facts.Edges)
}

func TestDocumentPeopleFactsSensitiveUnresolvedActor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
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
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE person_match_candidates SET display_name=$1 WHERE actor_key=$2`, "Private Name", "email:sender@example.test")
	require.NoError(t, err)
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:unmatched", "Unmatched", "", []PersonCandidateOccurrence{{ContentVersionID: version.ID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "missing"}}))
	safe, err := s.DocumentPeopleFacts(t.Context(), version.ID, true)
	require.NoError(t, err)
	foundSender := false
	for _, c := range safe.Candidates {
		if c.ActorKey == "email:sender@example.test" {
			require.Len(t, c.Occurrences, 1)
			require.Equal(t, "Private Name", c.DisplayName)
			require.Equal(t, "sender@example.test", c.OccurrenceLabels[c.Occurrences[0]])
			foundSender = true
		} else {
			require.Empty(t, c.Occurrences)
		}
	}
	require.True(t, foundSender)
	full, err := s.DocumentPeopleFacts(t.Context(), version.ID, false)
	require.NoError(t, err)
	require.Len(t, full.Candidates, 3)
	for _, c := range full.Candidates {
		if c.ActorKey == "email:sender@example.test" {
			for _, o := range c.Occurrences {
				if o.Role == "sender" {
					require.Equal(t, "sender@example.test", c.OccurrenceLabels[o])
				}
			}
		}
	}
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

func TestDocumentPeopleFactsSafeHiddenVisibilityUsesCallerSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
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
	visible, _ := ingestDocumentEventTarget(t, s, "visible.txt", "e2")
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, hideLocked, err := s.factsVersionTx(WithVisiblePeopleOnly(ctx), tx, visible.ID)
	require.NoError(t, err)
	require.True(t, hideLocked)
	var personVisible bool
	require.NoError(t, tx.QueryRowContext(ctx, visiblePersonByIDSQL, person.PersonID, boolInt(hideLocked)).Scan(&personVisible))
	require.False(t, personVisible)
	require.NoError(t, tx.Rollback())
	facts, err := s.DocumentPeopleFacts(ctx, photo.CurrentVersionID, true)
	require.NoError(t, err)
	require.True(t, facts.Persons[person.PersonID].Visible)
	_, err = s.DocumentPeopleFacts(WithVisiblePeopleOnly(ctx), photo.CurrentVersionID, true)
	require.ErrorIs(t, err, ErrNotFound)
	facts, err = s.DocumentPeopleFacts(WithVisiblePeopleOnly(WithPhotoHiddenToken(ctx, token)), photo.CurrentVersionID, true)
	require.NoError(t, err)
	require.True(t, facts.Persons[person.PersonID].Visible)
}
