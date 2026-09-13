package store

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
	"slices"
	"strings"
	"testing"
)

func TestEmailPurgeSharedGenerationAndPendingSuppression(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "first.eml")
	a, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "second.eml", a.Version.BlobHash, a.Version.Size, "message/rfc822")
	require.NoError(t, err)
	p := f.publication
	p.ContentVersionID = n.CurrentVersionID
	b, err := s.PublishEmailGeneration(t.Context(), p)
	require.NoError(t, err)
	pending, err := s.CreateFile(t.Context(), s.RootID(), "pending.eml", a.Version.BlobHash, a.Version.Size, "message/rfc822")
	require.NoError(t, err)
	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{a.Version.ID, pending.CurrentVersionID}})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedEmailHeads)
	require.Equal(t, 1, report.RemovedEmailAttachments)
	require.Zero(t, report.RemovedEmailGenerations)
	require.Zero(t, report.RemovedEmailPartArtifacts)
	_, err = s.EmailMetadata(t.Context(), a.Version.ID)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	_, err = s.EmailMetadata(t.Context(), pending.CurrentVersionID)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	_, err = s.PublishEmailGeneration(t.Context(), f.publication)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	_, err = s.EmailMetadata(t.Context(), b.Version.ID)
	require.NoError(t, err)
	for hash := range f.bytes {
		require.NotContains(t, report.PhysicalDerivativeBlobsPendingGC, hash)
	}
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	_, err = restored.EmailMetadata(t.Context(), a.Version.ID)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	report, err = s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedEmailGenerations)
	require.Equal(t, len(f.publication.Artifacts), report.RemovedEmailPartArtifacts)
	for hash := range f.bytes {
		if hash != a.Version.BlobHash {
			require.Contains(t, report.PhysicalDerivativeBlobsPendingGC, hash)
		}
	}
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailTargetsKeysetRetriesPendingBodiesAndSuppressionWins(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "first.eml")
	a, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	var ids []string
	ids = append(ids, a.Version.ID)
	for _, name := range []string{"second.eml", "third.eml"} {
		n, err := s.CreateFile(t.Context(), s.RootID(), name, a.Version.BlobHash, a.Version.Size, "MESSAGE/RFC822; charset=utf-8")
		require.NoError(t, err)
		ids = append(ids, n.CurrentVersionID)
	}
	whitespace, err := s.CreateFile(t.Context(), s.RootID(), "whitespace.eml",
		a.Version.BlobHash, a.Version.Size, "message/rfc822\t; charset=utf-8")
	require.NoError(t, err)
	ids = append(ids, whitespace.CurrentVersionID)
	slices.Sort(ids)
	recipe, err := document.EmailRecipeFingerprint(emailmime.Recipe())
	require.NoError(t, err)
	bodyProfile, err := emailBodyProfileRecord(emailmime.Recipe())
	require.NoError(t, err)
	var got []string
	after := ""
	for {
		targets, err := s.MissingEmailTargetsAfter(t.Context(), recipe, bodyProfile.Fingerprint, after, 1)
		require.NoError(t, err)
		if len(targets) == 0 {
			break
		}
		got = append(got, targets[0].Version.ID)
		after = targets[0].Version.ID
	}
	require.Equal(t, ids, got)
	for _, size := range []int{0, 101, -1} {
		_, err = s.MissingEmailTargetsAfter(t.Context(), recipe, bodyProfile.Fingerprint, "", size)
		require.Error(t, err)
	}
	bodyRecipe, err := document.EmailBodyRecipeFingerprint(a.Evidence.Recipe)
	require.NoError(t, err)
	require.NoError(t, s.RecordEmailBodyUnavailable(t.Context(), a.Attachment.ID, bodyRecipe, new("1.1"), "empty_body"))
	targets, err := s.MissingEmailTargetsAfter(t.Context(), recipe, bodyProfile.Fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 3)
	v, err := s.EmailMetadata(t.Context(), a.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "unavailable", v.BodySearch.State)
	require.Equal(t, "empty_body", *v.BodySearch.Reason)
	require.NoError(t, s.RecordEmailBodyUnavailable(t.Context(), a.Attachment.ID, bodyRecipe, new("1.1"), "empty_body"))
	require.Error(t, s.RecordEmailBodyUnavailable(t.Context(), a.Attachment.ID, bodyRecipe, new("1.1"), "unsupported_body"))
	require.Error(t, s.RecordEmailBodyUnavailable(t.Context(), a.Attachment.ID, bodyRecipe, nil, "network_failure"))
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.NoError(t, err)
	targets, err = s.MissingEmailTargetsAfter(t.Context(), fakeHash("ff"), bodyProfile.Fingerprint, "", 100)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestEmailTargetsSkipCompletedCorruptionAndContinueToPending(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "complete.eml")
	complete, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	bodyRecipe, err := document.EmailBodyRecipeFingerprint(complete.Evidence.Recipe)
	require.NoError(t, err)
	require.NoError(t, s.RecordEmailBodyUnavailable(
		t.Context(), complete.Attachment.ID, bodyRecipe, new("1.1"), "empty_body",
	))
	_, err = s.db.Exec(`UPDATE email_generations SET canonical_json=? WHERE generation_id=?`,
		[]byte(`{}`), complete.Generation.ID)
	require.NoError(t, err)
	pending, err := s.CreateFile(t.Context(), s.RootID(), "pending.eml",
		complete.Version.BlobHash, complete.Version.Size, "message/rfc822")
	require.NoError(t, err)
	recipe, err := document.EmailRecipeFingerprint(emailmime.Recipe())
	require.NoError(t, err)
	bodyProfile, err := emailBodyProfileRecord(emailmime.Recipe())
	require.NoError(t, err)

	targets, err := s.MissingEmailTargetsAfter(t.Context(), recipe, bodyProfile.Fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, pending.CurrentVersionID, targets[0].Version.ID)
}

func TestEmailTargetQueryUsesMIMEPartialIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN "+missingEmailVersionIDsQuery, "", fakeHash("recipe"))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	used := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		used = used || strings.Contains(detail, "content_versions_email_mime")
	}
	require.NoError(t, rows.Err())
	require.True(t, used, "email target query must use the MIME partial index")
}

func TestEmailAuditPurgeRollbackReplayAndTamper(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "audited.eml")
	v, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	var before bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &before))
	_, err = s.db.Exec(`CREATE TRIGGER fail_email_audit BEFORE INSERT ON audit_records BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END`)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.ErrorContains(t, err, "synthetic audit failure")
	_, err = s.db.Exec(`DROP TRIGGER fail_email_audit`)
	require.NoError(t, err)
	var after bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &after))
	require.Equal(t, before.String(), after.String())
	_, err = s.EmailMetadata(t.Context(), v.Version.ID)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var purged bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &purged))
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.NoError(t, err)
	var repeated bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &repeated))
	require.Equal(t, purged.String(), repeated.String(), "same inventory purge must not append another pending suppression")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(purged.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	_, err = restored.EmailMetadata(t.Context(), v.Version.ID)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	_, err = restored.db.Exec(`UPDATE derivative_purge_suppressions SET active=0,superseded_at=?,superseding_build_id=? WHERE profile_fingerprint=?`, "2026-09-09T12:00:00.000000000Z", fakeHash("ea"), derivativeAttachmentSuppressionScope(v.Version.ID, emailInventorySuppressionProfile))
	require.NoError(t, err)
	require.Error(t, restored.ValidateMetadata(t.Context()), "unrecorded suppression rewrite must fail audit replay")
}

func TestEmailVersionPruneAndLastTrashLeaveOrphansForCollection(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "first.eml")
	a, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "second.eml", a.Version.BlobHash, a.Version.Size, "message/rfc822")
	require.NoError(t, err)
	p := f.publication
	p.ContentVersionID = n.CurrentVersionID
	_, err = s.PublishEmailGeneration(t.Context(), p)
	require.NoError(t, err)
	first, err := s.NodeByID(t.Context(), a.Version.NodeID)
	require.NoError(t, err)
	first, _, err = s.ReplaceContent(t.Context(), first.ID, first.Revision, fakeHash("de"), 8, "text/plain")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), first.ID, first.Revision, VersionPruneSelector{VersionIDs: []string{a.Version.ID}}, true)
	require.NoError(t, err)
	_, err = s.EmailMetadata(t.Context(), a.Version.ID)
	require.ErrorIs(t, err, ErrNotFound)
	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	require.Zero(t, report.RemovedEmailGenerations)
	_, _, err = s.Trash(t.Context(), n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	report, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedEmailGenerations)
	require.Equal(t, len(f.publication.Artifacts), report.RemovedEmailPartArtifacts)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
