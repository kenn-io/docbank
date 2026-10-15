package store

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

func TestMediaMetadataExportKeepsIdentityAndDropsPrivateRefs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('s','remote_recording','cap.cloud','app',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	var a, b bytes.Buffer
	require.NoError(t, exportMetadataSnapshot(t.Context(), tx, &a))
	require.NoError(t, exportMetadataSnapshot(t.Context(), tx, &b))
	require.Equal(t, a.String(), b.String())
	require.Contains(t, a.String(), `"type":"media_source"`)
	require.NotContains(t, a.String(), "request_url")
}

func TestMediaMetadataRoundTripSanitizesRuntimeAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	recording, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	caption, err := s.CreateFile(ctx, s.RootID(), "caption.vtt", fakeHash("b2"), 20, "text/vtt")
	require.NoError(t, err)
	stamp := nowRFC3339()
	opID := "00000000-0000-4000-8000-000000000010"
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','remote_recording','cap.cloud','app',?,?)`,
		strings.Repeat("c", 64), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?)`,
		recording.CurrentVersionID, `{}`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
		'occurrence','source','source-version','operator','remote-ref','1','recording.mp3','','','{}',1,?,NULL)`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
		'input','occurrence','source','source-version',?,'caption','supplied','cap.cloud','en',?,?)`,
		caption.CurrentVersionID, fakeHash("b2"), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_operations VALUES(?, 'operator','submit_remote_recording',?,
		'source','{"outcome":"queued"}',?,?)`, opID, strings.Repeat("d", 64), stamp, stamp)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	for _, table := range []string{
		"media_sources", "media_source_versions", "media_occurrences", "media_input_artifacts", "media_operations",
	} {
		var count int
		require.NoError(t, restored.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 1, count, table)
	}
}

// TestMediaMetadataRestoreKeepsProcessingReceipts catches restore losing a
// recording's newest version or a recorded processing outcome, or resuming
// admission that never bound a job.
func TestMediaMetadataRestoreKeepsProcessingReceipts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	request := suppliedMediaPublicationFixture(t, s)
	consent := testProviderAuthorizationRequest()
	consent.Principal = request.Operation.Principal
	_, err := s.GrantConsent(ctx, grantRequestForAuthorization(consent, nil))
	require.NoError(t, err)
	authorization, err := s.AuthorizeProviderOperation(ctx, consent)
	require.NoError(t, err)
	consent.PriorAuthorization = &authorization
	request.ProcessingProfile, request.ProcessingPrincipal = "speech", consent.Principal
	request.ProcessingScope, request.ProcessingProfileFingerprint = consent.Scope, consent.ProfileFingerprint
	request.ProcessingAuthorization = consent
	noJob, err := s.RetainSuppliedMedia(ctx, request)
	require.NoError(t, err)
	bound := request.Operation
	bound.ID, bound.Verb = "00000000-0000-4000-8000-000000000071", "retry_media"
	boundReceipt := noJob
	boundReceipt.OperationID = bound.ID
	_, err = s.QueueMediaRetry(ctx, bound, boundReceipt)
	require.NoError(t, err)
	boundReceipt, err = s.SetMediaProcessingJob(ctx, bound.ID, bound.Principal, testSHA256([]byte("processing-job")))
	require.NoError(t, err)
	finished := bound
	finished.ID = "00000000-0000-4000-8000-000000000072"
	finishedReceipt := noJob
	finishedReceipt.OperationID = finished.ID
	_, err = s.QueueMediaRetry(ctx, finished, finishedReceipt)
	require.NoError(t, err)
	_, err = s.SetMediaProcessingJob(ctx, finished.ID, finished.Principal, testSHA256([]byte("finished-job")))
	require.NoError(t, err)
	finishedReceipt, err = s.FinishMediaProcessing(ctx, finished.ID, finished.Principal, true)
	require.NoError(t, err)

	sourceID := testSHA256([]byte("remote-source"))
	reference, _ := remoteStoreReference(t, s, sourceID, "00000000-0000-4000-8000-000000000821", "remote-a")
	_, err = s.RetainRemoteRecordingMedia(ctx, remoteStoreArtifact("00000000-0000-4000-8000-000000000822", sourceID,
		reference.OccurrenceID, "", testSHA256([]byte("remote-input-a")), testSHA256([]byte("remote-original-a")), 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true}))
	require.NoError(t, err)
	require.NoError(t, declareTestOccurrence(ctx, s, MediaOccurrenceInput{ID: "remote-b", SourceID: sourceID,
		Principal: "operator:remote", Ref: "remote-b", Revision: "1", MessageJSON: "{}"}))
	latest, err := s.RetainRemoteRecordingMedia(ctx, remoteStoreArtifact("00000000-0000-4000-8000-000000000823", sourceID,
		"remote-b", "", testSHA256([]byte("remote-input-b")), testSHA256([]byte("remote-original-b")), 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true}))
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &exported))
	status, err := restored.MediaSource(ctx, "operator:remote", sourceID)
	require.NoError(t, err)
	require.Equal(t, latest.SourceVersionID, status.SourceVersionID)
	require.Equal(t, latest.ContentVersionID, status.ContentVersionID)
	replay := func(op MediaOperation) MediaPublicationReceipt {
		raw, err := restored.MediaOperationReceipt(ctx, op)
		require.NoError(t, err)
		receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(raw))
		require.NoError(t, err)
		return receipt
	}
	restoredNoJob := replay(request.Operation)
	require.Equal(t, []string{"failed", "unavailable"}, []string{restoredNoJob.OperationState, restoredNoJob.CoverageState})
	require.Equal(t, boundReceipt, replay(bound), "a receipt with a job derives its state from that job")
	require.Equal(t, finishedReceipt, replay(finished), "a recorded outcome survives restore")
	pending, err := restored.MediaProcessingContinuations(ctx, "operator", "", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "only the job-bound queued receipt awaits an outcome")
	require.Equal(t, bound.ID, pending[0].OperationID)
}

func TestMediaMetadataMissingCurrentTableFailsValidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.Exec(`DROP TABLE media_input_artifacts`)
	require.NoError(t, err)
	err = s.ExportMetadata(t.Context(), &bytes.Buffer{})
	require.ErrorContains(t, err, "media_input_artifacts")
}

func TestPruneContentVersionsRetainsMediaOriginalAndInputAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	updated, _, err := s.ReplaceContent(ctx, created.ID, created.Revision, fakeHash("b2"), 20, "audio/mpeg")
	require.NoError(t, err)
	stamp := nowRFC3339()
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("c", 64), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?)`,
		created.CurrentVersionID, `{}`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
		'occurrence','source','source-version','operator','ref','1','','','','{}',1,?,NULL)`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
		'input','occurrence','source','source-version',?,'recording','supplied','','',?,?)`,
		created.CurrentVersionID, fakeHash("a1"), stamp)
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, false)
	require.NoError(t, err)
	require.Empty(t, preview.Candidates)
	require.Len(t, preview.DependencyRetained, 1)
	require.Equal(t, created.CurrentVersionID, preview.DependencyRetained[0].ID)

	run, err := s.PruneContentVersions(ctx, created.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
	require.NoError(t, err)
	require.False(t, run.Changed)
	_, err = s.ContentVersionByID(ctx, created.CurrentVersionID)
	require.NoError(t, err)
}

func TestPruneContentVersionsRetainsMediaRevertAncestryIncludingAllPrior(t *testing.T) {
	t.Parallel()
	for _, allPrior := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit selection", true: "all prior"}[allPrior], func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			created, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
			require.NoError(t, err)
			replaced, replacement, err := s.ReplaceContent(
				ctx, created.ID, created.Revision, fakeHash("b2"), 20, "audio/mpeg")
			require.NoError(t, err)
			reverted, revertVersion, _, err := s.RevertContent(
				ctx, created.ID, replaced.Revision, created.CurrentVersionID)
			require.NoError(t, err)
			_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
				strings.Repeat("c", 64), nowRFC3339())
			require.NoError(t, err)
			require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
				ID: "source-version", SourceID: "source", Revision: 1,
				ContentVersionID: revertVersion.ID, CaptureJSON: "{}",
			}))

			selector := VersionPruneSelector{AllPrior: true}
			node := reverted
			selected := []string{created.CurrentVersionID, replacement.ID, revertVersion.ID}
			if !allPrior {
				node, _, err = s.ReplaceContent(
					ctx, created.ID, reverted.Revision, fakeHash("d4"), 40, "audio/mpeg")
				require.NoError(t, err)
				selector = VersionPruneSelector{VersionIDs: selected}
			}

			preview, err := s.PruneContentVersions(ctx, created.ID, node.Revision, selector, false)
			require.NoError(t, err)
			require.Equal(t, []string{replacement.ID}, contentVersionIDs(preview.Candidates))
			require.ElementsMatch(t,
				[]string{created.CurrentVersionID, revertVersion.ID},
				contentVersionIDs(preview.DependencyRetained))
			require.False(t, preview.CheckpointRequired)

			run, err := s.PruneContentVersions(ctx, created.ID, node.Revision, selector, true)
			require.NoError(t, err)
			require.Equal(t, preview.Candidates, run.Candidates)
			require.Equal(t, preview.DependencyRetained, run.DependencyRetained)
			require.Equal(t, 1, run.DeletedVersions)
			require.Nil(t, run.Checkpoint)
			for _, id := range []string{created.CurrentVersionID, revertVersion.ID} {
				_, err = s.ContentVersionByID(ctx, id)
				require.NoError(t, err)
			}
			_, err = s.ContentVersionByID(ctx, replacement.ID)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}

	t.Run("all prior with historical pinned revert", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		created, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
		require.NoError(t, err)
		replaced, replacement, err := s.ReplaceContent(
			ctx, created.ID, created.Revision, fakeHash("b2"), 20, "audio/mpeg")
		require.NoError(t, err)
		reverted, pinnedRevert, _, err := s.RevertContent(
			ctx, created.ID, replaced.Revision, created.CurrentVersionID)
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
			strings.Repeat("c", 64), nowRFC3339())
		require.NoError(t, err)
		require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
			ID: "source-version", SourceID: "source", Revision: 1,
			ContentVersionID: pinnedRevert.ID, CaptureJSON: "{}",
		}))
		updated, replacementAfterRevert, err := s.ReplaceContent(
			ctx, created.ID, reverted.Revision, fakeHash("d4"), 40, "audio/mpeg")
		require.NoError(t, err)
		current, currentRevert, _, err := s.RevertContent(
			ctx, created.ID, updated.Revision, replacement.ID)
		require.NoError(t, err)

		selector := VersionPruneSelector{AllPrior: true}
		preview, err := s.PruneContentVersions(ctx, created.ID, current.Revision, selector, false)
		require.NoError(t, err)
		require.ElementsMatch(t,
			[]string{currentRevert.ID, replacementAfterRevert.ID, replacement.ID},
			contentVersionIDs(preview.Candidates))
		require.ElementsMatch(t,
			[]string{pinnedRevert.ID, created.CurrentVersionID},
			contentVersionIDs(preview.DependencyRetained))
		require.True(t, preview.CheckpointRequired)

		run, err := s.PruneContentVersions(ctx, created.ID, current.Revision, selector, true)
		require.NoError(t, err)
		require.Equal(t, preview.Candidates, run.Candidates)
		require.Equal(t, preview.DependencyRetained, run.DependencyRetained)
		require.Equal(t, 3, run.DeletedVersions)
		require.NotNil(t, run.Checkpoint)
		require.Equal(t, replacement.BlobHash, run.Checkpoint.BlobHash)
		for _, id := range []string{pinnedRevert.ID, created.CurrentVersionID} {
			_, err = s.ContentVersionByID(ctx, id)
			require.NoError(t, err)
		}
		for _, id := range []string{currentRevert.ID, replacementAfterRevert.ID, replacement.ID} {
			_, err = s.ContentVersionByID(ctx, id)
			require.ErrorIs(t, err, ErrNotFound)
		}
	})
}

func contentVersionIDs(versions []ContentVersion) []string {
	ids := make([]string, len(versions))
	for index, version := range versions {
		ids[index] = version.ID
	}
	return ids
}

func TestMediaMetadataRejectsNonObjectTimestampClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	stamp := nowRFC3339()
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("c", 64), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?)`,
		file.CurrentVersionID, `[]`, stamp)
	require.NoError(t, err)
	err = s.ExportMetadata(ctx, &bytes.Buffer{})
	require.ErrorContains(t, err, "capture claim")
}

func TestMediaInputArtifactBytesBelongToBackupAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	caption, err := s.CreateFile(ctx, s.RootID(), "caption.vtt", fakeHash("b2"), 20, "text/vtt")
	require.NoError(t, err)
	stamp := nowRFC3339()
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','remote_recording','cap.cloud','app',?,?)`,
		strings.Repeat("c", 64), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
		'occurrence','source',NULL,'operator','ref','1','','','','{}',1,?,NULL)`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
		'input','occurrence','source',NULL,?,'caption','supplied','cap.cloud','en',?,?)`,
		caption.CurrentVersionID, fakeHash("b2"), stamp)
	require.NoError(t, err)
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	var count int
	require.NoError(t, snapshot.QueryRowContext(ctx, BackupBlobAuthorityCTE()+`
		SELECT count(*) FROM backup_authorized_blobs WHERE hash=?`, fakeHash("b2")).Scan(&count))
	require.Equal(t, 1, count)
}

func TestMediaMetadataImportRequiresPristineMediaTables(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	target := newTestStore(t)
	_, err := target.db.Exec(`INSERT INTO media_sources VALUES('existing','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	err = target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes()))
	require.ErrorContains(t, err, "not pristine")
}
