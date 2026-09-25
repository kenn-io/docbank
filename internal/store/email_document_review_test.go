package store

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmailDocumentsBulkPurgeSkipsRetainedInventory(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	retained := newEmailFixture(t, s, "retained.eml")
	view, err := s.PublishEmailGeneration(t.Context(), retained.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "retain-inventory"))
	require.NoError(t, err)
	unrelated := newEmailSourceFixture(t, s, "unrelated.eml", "Subject: Unrelated\r\n\r\nUnrelated body")
	unrelatedView, err := s.PublishEmailGeneration(t.Context(), unrelated.publication)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{view.Version.ID}})
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	targetedErr := err
	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{All: true})
	require.NoError(t, err)
	require.ErrorContains(t, targetedErr, receipt.OperationID)
	require.Equal(t, 1, report.RemovedEmailAttachments)
	_, err = s.EmailMetadata(t.Context(), unrelatedView.Version.ID)
	require.ErrorIs(t, err, ErrEmailDerivativeSuppressed)
	_, err = s.EmailMetadata(t.Context(), view.Version.ID)
	require.NoError(t, err)
	got, err := s.EmailDocumentPublication(t.Context(), receipt.OperationID)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailDocumentsProcessingClassifiesInvalidAndStaleRequests(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "processing-errors"))
	require.NoError(t, err)
	child := receipt.Relations[0].Child
	profile := catalogProcessingProfile(t, false)
	job := renditionJobTestRequest(child.VersionID, profile)
	job.ExecutionIdentity.Upload.SHA256 = child.SHA256
	job.ExecutionIdentity.Upload.ByteLength = child.Size
	job.ExecutionIdentity.Authorization.SourceSHA256 = child.SHA256
	job.ExecutionIdentity.Authorization.SourceBytes = child.Size
	grantRenditionJobConsent(t, s, job)
	var p document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &p))
	request := document.EmailDocumentProcessingRequest{
		OperationID: receipt.OperationID, RequestDigest: receipt.RequestDigest, Order: 1,
		Profile: p, ExecutionIdentity: job.ExecutionIdentity, CapturedArtifactPolicy: job.CapturedArtifactPolicy,
		Principal: job.Authorization.Principal, Scope: job.Authorization.Scope,
		InputClasses: job.Authorization.InputClasses, RetainedArtifactClasses: job.Authorization.RetainedArtifactClasses,
	}
	_, err = s.RequestEmailDocumentProcessing(t.Context(), request)
	require.NoError(t, err)
	for _, change := range []string{"policy", "identity", "profile binding", "principal", "input classes", "retained classes"} {
		t.Run(change, func(t *testing.T) {
			bad := request
			switch change {
			case "policy":
				bad.CapturedArtifactPolicy = []byte(`{}`)
			case "identity":
				bad.ExecutionIdentity.Upload.CapabilityRecordChecksum = "invalid"
			case "profile binding":
				bad.ExecutionIdentity.Authorization.RenditionRequestFingerprint = fakeHash("ff")
			case "principal":
				bad.Principal = ""
			case "input classes":
				bad.InputClasses = []string{"other_input"}
			case "retained classes":
				bad.RetainedArtifactClasses = nil
			}
			_, err := s.RequestEmailDocumentProcessing(t.Context(), bad)
			require.ErrorIs(t, err, ErrInvalidEmailDocumentRequest)
		})
	}
	_, err = s.db.Exec(`CREATE TRIGGER fail_email_job BEFORE INSERT ON rendition_jobs BEGIN SELECT RAISE(ABORT,'synthetic job write failure'); END`)
	require.NoError(t, err)
	_, err = s.RequestEmailDocumentProcessing(t.Context(), request)
	require.ErrorContains(t, err, "synthetic job write failure")
	require.NotErrorIs(t, err, ErrInvalidEmailDocumentRequest)
	require.NotErrorIs(t, err, ErrEmailDocumentConflict)
	_, _, err = s.ReplaceContent(t.Context(), child.NodeID, UnconditionalRev, fakeHash("ed"), 3, "text/plain")
	require.NoError(t, err)
	_, err = s.RequestEmailDocumentProcessing(t.Context(), request)
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
}

func TestEmailDocumentsProcessingReportsNativeExtractionOutcomes(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"indexed", "none", "failed"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			f := newEmailSourceFixture(t, s, "source.eml", "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=source.txt\r\n\r\nattachmentuniqueterm\r\n--m--\r\n")
			view, err := s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
			receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "native-status"))
			require.NoError(t, err)
			child := receipt.Relations[0].Child
			query := document.EmailDocumentRelationQuery{ChildVersionID: child.VersionID}
			page, err := s.EmailDocumentRelations(t.Context(), query)
			require.NoError(t, err)
			require.Equal(t, "pending", page.Items[0].State)
			result := ExtractionResult{BlobHash: child.SHA256, Extractor: legacyPlainTextExtractor, ExtractorVersion: legacyPlainTextExtractorVersion, Status: ExtractionOK}
			switch state {
			case "indexed":
				result.Text = "attachmentuniqueterm"
			case "failed":
				result.Status, result.Error = ExtractionFailed, "synthetic extraction failure"
			}
			require.NoError(t, s.RecordExtraction(t.Context(), result))
			page, err = s.EmailDocumentRelations(t.Context(), query)
			require.NoError(t, err)
			require.Equal(t, state, page.Items[0].State)
			hits, _, err := s.SearchPage(t.Context(), "attachmentuniqueterm", 10)
			require.NoError(t, err)
			if state == "indexed" {
				require.Len(t, hits, 1)
			} else {
				require.Empty(t, hits)
			}
		})
	}
}

func TestEmailDocumentsProcessingReportsServingRenditionOutcomes(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"complete", "partial", "truncated", "partial_success", "none"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			f := newEmailFixture(t, s, "source.eml")
			view, err := s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
			receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "rendition-status"))
			require.NoError(t, err)
			child, err := s.NodeByID(t.Context(), receipt.Relations[0].Child.NodeID)
			require.NoError(t, err)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				for _, hash := range []string{catalogEvidenceBlobHash, catalogMarkdownBlobHash} {
					if err := s.EnsureBlobTx(tx, hash, int64(len(catalogBlobContents[hash]))); err != nil {
						return err
					}
				}
				return nil
			}))
			profile := catalogProcessingProfile(t, false)
			request := renditionJobTestRequest(child.CurrentVersionID, profile)
			request.ExecutionIdentity.Upload.SHA256 = child.BlobHash
			request.ExecutionIdentity.Upload.ByteLength = child.Size
			request.ExecutionIdentity.Authorization.SourceSHA256 = child.BlobHash
			request.ExecutionIdentity.Authorization.SourceBytes = child.Size
			grantRenditionJobConsent(t, s, request)
			job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			at := time.Now().UTC().Add(time.Second)
			claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "email-status-worker", at, time.Minute)
			require.NoError(t, err)
			_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID, at.Add(time.Second), renditionJobTestSnapshot(request))
			require.NoError(t, err)
			build := catalogRenditionBuild(s, profile)
			build.ID, build.SourceSHA256 = job.ID, child.BlobHash
			switch state {
			case "partial":
				build.Completeness = document.EvidencePartial
			case "truncated":
				build.Truncated = true
			case "partial_success":
				build.PartialSuccess = true
			case "none":
				build.Units, build.LexicalSegments = nil, nil
			}
			require.NoError(t, s.StageRenditionJobBuild(t.Context(), claim, build, at.Add(2*time.Second)))
			_, err = s.StageRenditionJobGeneration(t.Context(), claim, fakeHash("bc"), at.Add(3*time.Second))
			require.NoError(t, err)
			_, err = s.PublishRenditionJob(t.Context(), claim, at.Add(4*time.Second))
			require.NoError(t, err)
			// Relations retain exact historical versions after the ordinary child changes.
			_, _, err = s.ReplaceContent(t.Context(), child.ID, child.Revision, fakeHash("ce"), 3, "text/plain")
			require.NoError(t, err)
			page, err := s.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: child.CurrentVersionID})
			require.NoError(t, err)
			want := "partial"
			switch state {
			case "complete":
				want = "indexed"
			case "none":
				want = "none"
			}
			require.Equal(t, want, page.Items[0].State)
		})
	}
}

func TestEmailDocumentsRepublishReleasedReceiptReportsConflict(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "released-operation"))
	require.NoError(t, err)
	require.NoError(t, s.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, receipt.RequestDigest))
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, receipt.OperationID))
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	require.ErrorContains(t, err, receipt.OperationID)
	_, err = s.EmailDocumentPublication(t.Context(), receipt.OperationID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ContentVersionByID(t.Context(), receipt.Relations[0].Child.VersionID)
	require.NoError(t, err)
	page, err := s.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ParentVersionID: view.Version.ID})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
