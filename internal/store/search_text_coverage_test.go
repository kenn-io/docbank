package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSearchTextCoverage(t *testing.T) {
	t.Parallel()
	s, _, pdfs, profile := collectionCoverageFixture(t, 1)
	_, err := s.CreateFile(t.Context(), s.RootID(), "untyped", testSHA256([]byte("untyped")), 7, "")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "message.eml", testSHA256([]byte("email")), 5, "message/rfc822")
	require.NoError(t, err)
	text, err := s.CreateFile(t.Context(), s.RootID(), "notes.txt", testSHA256([]byte("notes")), 5, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.DeferTextExtraction(t.Context(), text.BlobHash, time.Now().Add(time.Hour)))
	got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{Families: []string{}, Pending: 1}, got)
	emptyDir, err := s.Mkdir(t.Context(), s.RootID(), "empty")
	require.NoError(t, err)
	outside, err := s.SearchTextCoverage(t.Context(), SearchOptions{UnderNodeID: emptyDir.ID}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{Families: []string{}}, outside)
	// Shared bytes are two files, while the worker's queue contains one blob.
	duplicate, err := s.CreateFile(t.Context(), s.RootID(), "copy.txt", text.BlobHash, 5, "text/plain")
	require.NoError(t, err)
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{MIMEType: "text/plain"}, "", nil)
	require.NoError(t, err)
	require.Equal(t, 2, got.Pending)
	_, _, err = s.Trash(t.Context(), duplicate.ID, duplicate.Revision)
	require.NoError(t, err)
	collectionCoveragePublish(t, s, pdfs[0], profile, "complete")
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{Families: []string{}, Pending: 1}, got)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: text.BlobHash,
		Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "lease renewal"}))
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{Families: []string{}}, got)
	// An exact replacement does not inherit its old rendition.
	_, _, err = s.ReplaceContent(t.Context(), pdfs[0].ID, pdfs[0].Revision, pdfs[0].BlobHash, pdfs[0].Size, pdfs[0].MimeType)
	require.NoError(t, err)
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{MIMEType: "application/pdf"}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{Families: []string{}}, got)
	_, err = s.CreateFile(t.Context(), s.RootID(), "slides.pptx", testSHA256([]byte("slides")), 6, "application/vnd.openxmlformats-officedocument.presentationml.presentation")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "photo.png", testSHA256([]byte("photo")), 5, "image/png")
	require.NoError(t, err)
	failed, err := s.CreateFile(t.Context(), s.RootID(), "invalid.txt", testSHA256([]byte("invalid")), 7, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: failed.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionFailed, Error: "invalid UTF-8"}))
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{NoText: 1, Families: []string{"office"}}, got)
	t.Run("blank_segments", func(t *testing.T) {
		t.Parallel()
		s, _, pdfs, profile := collectionCoverageFixture(t, 1)
		collectionCoveragePublish(t, s, pdfs[0], profile, "blank_segments")
		got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, func(context.Context, string) (bool, error) { return true, nil })
		require.NoError(t, err)
		require.Equal(t, SearchTextCoverage{Families: []string{}}, got)
	})
	t.Run("pdf waiting and terminal", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		versions := seedRenditionCatalogVersions(t, s)
		profile := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
			profile.Rendition.Descriptor.ID = document.PDFTextProviderID
		})
		coverage, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
		require.NoError(t, err)
		require.Zero(t, coverage.Pending)
		require.Zero(t, coverage.NoText)
		request := renditionJobTestRequest(versions[0], profile)
		request.ExecutionIdentity.Authorization.ProviderID = document.PDFTextProviderID
		grantRenditionJobConsent(t, s, request)
		job, _, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
		require.NoError(t, err)
		require.Equal(t, 1, coverage.Pending)
		now := time.Now().UTC().Add(time.Second)
		claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "pdf-worker", now, time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, now))
		coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
		require.NoError(t, err)
		require.Zero(t, coverage.Pending)
		require.Zero(t, coverage.NoText)
	})
	for _, tc := range []struct {
		name    string
		active  bool
		pending int
	}{
		{name: "native"},
		{name: "rendition", active: true, pending: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, pdfs, profile := collectionCoverageFixture(t, 1)
			if tc.active {
				collectionCoveragePublish(t, s, pdfs[0], profile, "complete")
			}
			text, err := s.CreateFile(t.Context(), s.RootID(), "notes.txt", testSHA256([]byte("notes")), 5, "text/plain")
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: text.BlobHash,
				Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "lease renewal"}))
			_, err = s.CreateFile(t.Context(), s.RootID(), "copy.txt", text.BlobHash, 5, "text/plain")
			require.NoError(t, err)
			got, err := s.SearchTextCoverage(t.Context(), SearchOptions{MIMEType: "text/plain"}, "", nil)
			require.NoError(t, err)
			require.Equal(t, tc.pending, got.Pending)
		})
	}
}
