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
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?,?,?,?)`,
		recording.CurrentVersionID, fakeHash("a1"), 10, `{}`, digestCatalogJSON([]byte(`{}`)), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_source_heads VALUES('source','source-version',1)`)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
		'occurrence','source','source-version','operator','remote-ref','1','recording.mp3','','','{}',1,?,NULL)`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_visibility_fences VALUES('operator',1,?)`, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
		'input','occurrence','source','source-version',?,'caption','supplied','cap.cloud','en',?,?)`,
		caption.CurrentVersionID, fakeHash("b2"), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_operations VALUES(?, 'operator','submit_remote_recording',?,
		'queued','source','{"outcome":"queued"}',?,?)`, opID, strings.Repeat("d", 64), stamp, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_acquisitions VALUES(
		'acquisition',?,'occurrence','source','cap-origin',?,?,?,'running','download','','',1,
		'worker',1,?,?,0,?,NULL)`, opID, strings.Repeat("e", 64), strings.Repeat("f", 64),
		`{"grant":"secret-authorization"}`, stamp, stamp, stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_protected_refs VALUES(
		'acquisition','occurrence','https://private.example/share/token','credential-secret',?)`, stamp)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	for _, kind := range []string{
		"media_source", "media_source_version", "media_source_head", "media_occurrence",
		"media_visibility_fence", "media_input_artifact", "media_operation", "media_acquisition_receipt",
	} {
		require.Contains(t, exported.String(), `"type":"`+kind+`"`)
	}
	require.NotContains(t, exported.String(), "private.example")
	require.NotContains(t, exported.String(), "credential-secret")
	require.NotContains(t, exported.String(), "secret-authorization")

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	for _, table := range []string{
		"media_sources", "media_source_versions", "media_source_heads", "media_occurrences",
		"media_visibility_fences", "media_input_artifacts", "media_operations", "media_acquisitions",
	} {
		var count int
		require.NoError(t, restored.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 1, count, table)
	}
	var protected int
	require.NoError(t, restored.db.QueryRow(`SELECT count(*) FROM media_protected_refs`).Scan(&protected))
	require.Zero(t, protected)
	var operationState, acquisitionState, outcome, failureCode, authorization string
	var claimOwner, leaseExpires sql.NullString
	require.NoError(t, restored.db.QueryRow(`SELECT o.state,a.state,a.outcome,a.failure_code,
		a.authorization_json,a.claim_owner,a.lease_expires_at FROM media_operations o
		JOIN media_acquisitions a ON a.operation_id=o.operation_id`).Scan(
		&operationState, &acquisitionState, &outcome, &failureCode, &authorization, &claimOwner, &leaseExpires))
	require.Equal(t, "failed", operationState)
	require.Equal(t, "failed", acquisitionState)
	require.Equal(t, "access_required", outcome)
	require.Equal(t, "consent_absent", failureCode)
	require.JSONEq(t, `{}`, authorization)
	require.False(t, claimOwner.Valid)
	require.False(t, leaseExpires.Valid)
}

func TestMediaMetadataRestoreTerminatesProcessingReceipts(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"submit_supplied_media", "retry_media"} {
		for _, completed := range []bool{false, true} {
			name := verb + "/pending"
			if completed {
				name = verb + "/completed"
			}
			t.Run(name, func(t *testing.T) {
				s := newTestStore(t)
				request := suppliedMediaPublicationFixture(t, s)
				consent := testProviderAuthorizationRequest()
				consent.Principal = request.Operation.Principal
				_, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(consent, nil))
				require.NoError(t, err)
				authorization, err := s.AuthorizeProviderOperation(t.Context(), consent)
				require.NoError(t, err)
				consent.PriorAuthorization = &authorization
				if verb == "submit_supplied_media" {
					request.ProcessingProfile = "speech"
					request.ProcessingPrincipal = consent.Principal
					request.ProcessingScope = consent.Scope
					request.ProcessingProfileFingerprint = consent.ProfileFingerprint
					request.ProcessingAuthorization = consent
				}
				receipt, err := s.RetainSuppliedMedia(t.Context(), request)
				require.NoError(t, err)
				operation := request.Operation
				if verb == "retry_media" {
					operation.ID = "00000000-0000-4000-8000-000000000071"
					operation.Verb = verb
					receipt.OperationID = operation.ID
					receipt.OperationState, receipt.CoverageState = "queued", "pending"
					receipt.ProcessingProfile = "speech"
					receipt.ProcessingPrincipal = consent.Principal
					receipt.ProcessingScope = consent.Scope
					receipt.ProcessingProfileFingerprint = consent.ProfileFingerprint
					receipt.ProcessingAuthorization = consent
					_, err = s.QueueMediaRetry(t.Context(), operation, receipt)
					require.NoError(t, err)
				}
				receipt, err = s.SetMediaProcessingJob(t.Context(), operation.ID, operation.Principal, testSHA256([]byte("processing-job")))
				require.NoError(t, err)
				if completed {
					receipt, err = s.FinishMediaProcessing(t.Context(), operation.ID, operation.Principal, true)
					require.NoError(t, err)
				}
				var exported bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &exported))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(t.Context(), &exported))
				replayed, err := restored.MediaOperationReceipt(t.Context(), operation)
				require.NoError(t, err)
				actual, err := canonical.Decode[MediaPublicationReceipt]([]byte(replayed))
				require.NoError(t, err)
				if !completed {
					receipt.OperationState, receipt.CoverageState, receipt.JobID = "failed", "unavailable", ""
				}
				require.Equal(t, receipt, actual, "replay must report the restored processing result")
				status, err := restored.MediaSource(t.Context(), operation.Principal, operation.SourceID)
				require.NoError(t, err)
				require.NotNil(t, status.ProcessingReceipt)
				require.Equal(t, receipt, *status.ProcessingReceipt, "status must use the same terminal receipt")
				pending, err := restored.MediaProcessingContinuations(t.Context(), 10)
				require.NoError(t, err)
				require.Empty(t, pending)
			})
		}
	}
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
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?,?,?,?)`,
		created.CurrentVersionID, fakeHash("a1"), 10, `{}`, digestCatalogJSON([]byte(`{}`)), stamp)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_source_heads VALUES('source','source-version',1)`)
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
				ContentVersionID: revertVersion.ID, SourceSHA256: created.BlobHash, SourceBytes: created.Size,
				CaptureJSON: "{}", ClaimSHA256: digestCatalogJSON([]byte("{}")),
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
			ContentVersionID: pinnedRevert.ID, SourceSHA256: created.BlobHash, SourceBytes: created.Size,
			CaptureJSON: "{}", ClaimSHA256: digestCatalogJSON([]byte("{}")),
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
	_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?,?,?,?)`,
		file.CurrentVersionID, fakeHash("a1"), 10, `[]`, digestCatalogJSON([]byte(`[]`)), stamp)
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
	_, err = s.db.Exec(`INSERT INTO media_visibility_fences VALUES('operator',1,?)`, stamp)
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
