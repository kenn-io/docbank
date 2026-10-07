package processing

import (
	"fmt"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func BenchmarkMediaSearchSelectors4096(b *testing.B) {
	for _, distinct := range []bool{false, true} {
		name := "shared_build"
		if distinct {
			name = "distinct_builds"
		}
		b.Run(name, func(b *testing.B) {
			f, request := mediaSearchSelectorWorkload(b, distinct)
			b.ResetTimer()
			for b.Loop() {
				report, err := f.service.Search(b.Context(), request)
				require.NoError(b, err)
				require.Len(b, report.Results, 100)
				require.Equal(b, 4096, report.Coverage.CompleteDocuments)
				require.Equal(b, 4096, report.Coverage.ScopedDocuments)
				require.True(b, report.Truncated)
			}
		})
	}
}

func mediaSearchSelectorWorkload(t testing.TB, distinct bool) (mediaStateFixture, SearchRequest) {
	f := newMediaStateFixture(t)
	retained, err := f.service.SubmitSuppliedMedia(t.Context(), f.suppliedRequest(uuid.New().String(), &MediaProcessingRequest{Profile: "speech"}))
	require.NoError(t, err)
	require.NoError(t, f.run(t, retained.JobID))
	active, err := f.catalog.ActiveRendition(t.Context(), f.version.ID, f.service.profiles["speech"].record.Fingerprint)
	require.NoError(t, err)
	generation, err := f.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	request := SearchRequest{Query: "Synthetic worker output", Mode: "lexical", Profile: "speech", Limit: 100, Fence: SourceFence{VaultUID: f.catalog.VaultID()}}
	attachments := make([]store.RenditionAttachmentRecord, 0, 4096)
	for i := 0; i < 4096; i++ {
		label := fmt.Sprintf("cost-%04d", i)
		node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), label+".wav", f.version.BlobHash, f.version.Size, "audio/wav")
		require.NoError(t, err)
		identity := processingSHA256([]byte(label))
		sourceID, err := store.MediaSourceKey("remote_recording", f.catalog.VaultID(), "synthetic", "cost", label)
		require.NoError(t, err)
		operationID, occurrenceID, sourceVersionID := uuid.New().String(), uuid.New().String(), uuid.New().String()
		_, err = f.catalog.RetainMediaReference(t.Context(), store.MediaReferencePublicationRequest{
			Operation: store.MediaOperation{ID: operationID, Principal: f.service.principal, Verb: "submit_remote_recording", RequestSHA256: identity, SourceID: sourceID},
			Provider:  "synthetic", OriginScope: "cost", IdentitySHA256: identity,
			Occurrence: store.MediaOccurrenceInput{ID: occurrenceID, SourceID: sourceID, Principal: f.service.principal, Ref: label, Revision: "1", Filename: label + ".wav", MessageJSON: "{}"}})
		require.NoError(t, err)
		require.NoError(t, f.catalog.PublishMediaSourceVersion(t.Context(), store.MediaSourceVersionInput{ID: sourceVersionID, SourceID: sourceID,
			ContentVersionID: node.CurrentVersionID, CaptureJSON: "{}", Revision: 1, BindOccurrenceIDs: []string{occurrenceID}}))
		attachment := active.Attachment
		if distinct {
			build := active.Build
			build.ID = processingSHA256([]byte("build-" + label))
			build.ProviderOperationID = "synthetic-" + label
			require.NoError(t, f.catalog.StageRenditionBuild(t.Context(), build))
			attachment.BuildID = build.ID
		}
		attachment.ID = store.RenditionAttachmentID(identity, node.CurrentVersionID, attachment.Profile.Fingerprint)
		attachment.ContentVersionID = node.CurrentVersionID
		attachments = append(attachments, attachment)
		operationID = uuid.New().String()
		_, err = f.catalog.QueueMediaRetry(t.Context(), store.MediaOperation{ID: operationID, Principal: f.service.principal, Verb: "retry_media", RequestSHA256: identity, SourceID: sourceID},
			store.MediaPublicationReceipt{VaultUID: f.catalog.VaultID(), SourceID: sourceID, SourceVersionID: sourceVersionID, ContentVersionID: node.CurrentVersionID,
				OccurrenceID: occurrenceID, OperationID: operationID, JobID: retained.JobID, OperationState: "queued", CoverageState: "pending", ProcessingNodeID: node.ID,
				ProcessingProfile: "speech", ProcessingProfileFingerprint: attachment.Profile.Fingerprint})
		require.NoError(t, err)
		request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, node.CurrentVersionID)
		request.MediaSources = append(request.MediaSources, retrieval.MediaSource{SourceID: sourceID, SourceVersionID: sourceVersionID, ContentVersionID: node.CurrentVersionID})
		if i%1024 == 1023 {
			t.Logf("fixture ready nodes=%d", i+1)
		}
	}
	if distinct {
		generation, err = f.catalog.StageLexicalGeneration(t.Context(), processingSHA256([]byte("distinct-generation")))
		require.NoError(t, err)
	}
	// Seed validated fixture heads together, then validate the complete generation once.
	db, err := store.DefaultSQLiteDriver().Open(f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, a := range attachments[:len(attachments)-1] {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO rendition_attachments(attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,retention_disclosure_fingerprint,attachment_policy_fingerprint,consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.VaultID, a.ContentVersionID, a.BuildID, a.Profile.Fingerprint, a.Profile.RetentionDisclosureFingerprint, a.Profile.AttachmentPolicyFingerprint, a.Profile.ConsentFingerprint, a.Profile.RenditionDisclosureFingerprint, a.Profile.TrustBoundary, a.AttachedAt)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at) VALUES(?,?,?,?)`, a.ContentVersionID, a.Profile.Fingerprint, a.ID, a.AttachedAt)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	a := attachments[len(attachments)-1]
	require.NoError(t, f.catalog.PublishRenditionAndLexicalHeads(t.Context(), a, store.RenditionHeadRecord{ContentVersionID: a.ContentVersionID, ProcessingProfileFingerprint: a.Profile.Fingerprint, AttachmentID: a.ID, PublishedAt: a.AttachedAt}, generation.ID))
	return f, request
}
