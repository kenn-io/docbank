package store

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoSearchRevalidationCompilesAfterPublication(t *testing.T) {
	t.Parallel()
	s, _ := newRenditionCatalogFixture(t)
	node := browsePhotoNode(t, s, "photo.jpg", browseHash("photo"), "image/jpeg")
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury harbor")
	build.SourceSHA256 = node.BlobHash
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	first, err := s.StageLexicalGeneration(t.Context(), fakeHash("91"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	head := RenditionHeadRecord{ContentVersionID: node.CurrentVersionID, ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z"}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), attachment, head, first.ID))
	second, err := s.StageLexicalGeneration(t.Context(), fakeHash("92"))
	require.NoError(t, err)
	publisher, err := Open(t.Context(), s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })

	// Reserve the consuming transaction's connection. Compilation used to run
	// before this wait, allowing publication to collect its selected generation.
	conn, err := s.writeDB.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	waits := s.writeDB.Stats().WaitCount
	scope := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`)}
	lexical := snapshotTestQuery(t, `{"text":"mercury"}`)
	candidates := []SearchCandidateIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Evidence: []SearchEvidenceIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Kind: "node_name", NodeName: node.Name}}}}
	done := make(chan struct {
		result SearchCandidateRevalidation
		err    error
	}, 1)
	go func() {
		result, err := s.RevalidateSearchCandidates(t.Context(), candidates, SearchOptions{PhotoScope: &scope, PhotoLexicalQuery: &lexical}, "", "")
		done <- struct {
			result SearchCandidateRevalidation
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.writeDB.Stats().WaitCount == waits && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	require.Greater(t, s.writeDB.Stats().WaitCount, waits)
	require.NoError(t, publisher.PublishRenditionAndLexicalHeads(t.Context(), attachment, head, second.ID))
	var remaining int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM rendition_lexical_generations WHERE generation_id=?`, first.ID).Scan(&remaining))
	require.Zero(t, remaining)
	require.NoError(t, conn.Close())
	got := <-done
	require.NoError(t, got.err)
	require.Len(t, got.result.Candidates, 1)
	require.Equal(t, node.ID, got.result.Candidates[0].NodeID)
}
