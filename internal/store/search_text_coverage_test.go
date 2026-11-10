package store

import (
	"context"
	"errors"
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
	require.Equal(t, SearchTextCoverage{NoText: 1, Families: []string{"pdf"}, Pending: 1}, got)
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
	require.Equal(t, SearchTextCoverage{NoText: 1, Families: []string{"pdf"}}, got)
	_, err = s.CreateFile(t.Context(), s.RootID(), "slides.pptx", testSHA256([]byte("slides")), 6, "application/vnd.openxmlformats-officedocument.presentationml.presentation")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "photo.png", testSHA256([]byte("photo")), 5, "image/png")
	require.NoError(t, err)
	failed, err := s.CreateFile(t.Context(), s.RootID(), "invalid.txt", testSHA256([]byte("invalid")), 7, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: failed.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionFailed, Error: "invalid UTF-8"}))
	got, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, "", nil)
	require.NoError(t, err)
	require.Equal(t, SearchTextCoverage{NoText: 2, Families: []string{"pdf", "office"}}, got)
	t.Run("blank_segments", func(t *testing.T) {
		t.Parallel()
		s, _, pdfs, profile := collectionCoverageFixture(t, 1)
		collectionCoveragePublish(t, s, pdfs[0], profile, "blank_segments")
		got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, func(context.Context, string) ([]int, error) { return []int{1}, nil })
		require.NoError(t, err)
		require.Equal(t, SearchTextCoverage{NoText: 1, Families: []string{"pdf"}}, got)
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
		require.Equal(t, 2, coverage.NoText)
		request := renditionJobTestRequest(versions[0], profile)
		request.ExecutionIdentity.Authorization.ProviderID = document.PDFTextProviderID
		grantRenditionJobConsent(t, s, request)
		job, _, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
		require.NoError(t, err)
		require.Equal(t, 1, coverage.Pending)
		require.Equal(t, 1, coverage.NoText)
		now := time.Now().UTC().Add(time.Second)
		claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "pdf-worker", now, time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, now))
		coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
		require.NoError(t, err)
		require.Zero(t, coverage.Pending)
		require.Equal(t, 2, coverage.NoText)
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

func TestSearchTextCoverageUnprocessedPDFGetsGuidance(t *testing.T) {
	s, _, _, profile := collectionCoverageFixture(t, 1)
	got, err := s.SearchTextCoverage(t.Context(), SearchOptions{MIMEType: "application/pdf"}, profile.Fingerprint, nil)
	require.NoError(t, err)
	require.Equal(t, 1, got.NoText)
	require.Equal(t, []string{"pdf"}, got.Families)
}

func TestSearchTextCoverageCandidateEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, state                     string
		pages                           []int
		readError                       bool
		generic, unavailable, pageCount int
	}{
		{"no readable text", "candidate_empty", nil, false, 1, 0, 0},
		{"missing evidence", "candidate_empty", nil, true, 1, 1, 0},
		{"searchable with missing evidence", "candidate_serving", nil, true, 0, 1, 0},
		{"known scan", "candidate_empty", []int{2, 4}, false, 0, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, pdfs, profile := collectionCoverageFixture(t, 1)
			collectionCoveragePublish(t, s, pdfs[0], profile, tc.state)
			got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, func(context.Context, string) ([]int, error) {
				if tc.readError {
					return nil, errors.New("evidence unavailable")
				}
				return tc.pages, nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.generic, got.NoText)
			require.Equal(t, tc.unavailable, got.PDFPageCoverageUnavailable)
			require.Equal(t, tc.pageCount, got.PagesWithoutText)
		})
	}
}

func TestSearchTextCoverageLimitsEvidenceReads(t *testing.T) {
	s, _, nodes, profile := collectionCoverageFixture(t, 17)
	for _, node := range nodes {
		collectionCoveragePublish(t, s, node, profile, "candidate_empty")
	}
	calls := 0
	got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, func(context.Context, string) ([]int, error) { calls++; return []int{2, 4}, nil })
	require.NoError(t, err)
	require.Equal(t, 16, calls)
	require.True(t, got.PDFPageCoveragePartial)
	require.Equal(t, 16, got.PDFsWithoutText)
	require.Equal(t, 32, got.PagesWithoutText)
	require.Zero(t, got.NoText)
}

func TestSearchTextCoverageOtherProfileServes(t *testing.T) {
	s, _, nodes, profile := collectionCoverageFixture(t, 1)
	collectionCoveragePublish(t, s, nodes[0], profile, "candidate_empty")
	other := catalogProcessingProfile(t, true)
	collectionCoveragePublish(t, s, nodes[0], other, "complete")
	got, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, func(context.Context, string) ([]int, error) { return nil, errors.New("missing") })
	require.NoError(t, err)
	require.Zero(t, got.PDFPageCoverageUnavailable)
	require.Zero(t, got.NoText)
}
