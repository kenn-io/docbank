package store

import (
	"database/sql"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestSearchPathChecksumUsesExactCanonicalPathBytes(t *testing.T) {
	assert.Equal(t, "797f517bd89c7ed92b9d1ec9e87d42fde8631f6932cfca9312d8a443fbbf582d",
		SearchPathChecksum("/docs/report.txt"))
	assert.NotEqual(t, SearchPathChecksum("/docs/report.txt"), SearchPathChecksum("/Docs/report.txt"))
}

func TestRevalidateSearchCandidatesPreservesOrderAtTheCandidateLimit(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.txt", fakeHash("first"), 5, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.txt", fakeHash("second"), 6, "text/plain")
	require.NoError(t, err)
	requested := make([]SearchCandidateIdentity, document.MaxRetrievalCandidateLimit)
	for i := range requested {
		node := first
		if i%2 == 0 {
			node = second
		}
		requested[i] = searchCandidateIdentity(t, s, node,
			SearchEvidenceIdentity{Kind: "node_name"})
	}

	revalidation, err := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, "", "")
	require.NoError(t, err)
	require.Len(t, revalidation.Candidates, len(requested))
	for i := range requested {
		assert.Equal(t, requested[i], revalidation.Candidates[i])
	}
}

func TestRevalidateSearchCandidatesRejectsInvalidPathChecksums(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "report.txt", fakeHash("report"), 6, "text/plain")
	require.NoError(t, err)
	base := searchCandidateIdentity(t, s, node, SearchEvidenceIdentity{Kind: "node_name"})

	for _, checksum := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		t.Run(checksum, func(t *testing.T) {
			candidate := base
			candidate.PathChecksum = checksum
			_, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate},
				SearchOptions{}, "", "")
			require.ErrorContains(t, err, "path checksum")
		})
	}
}

func TestRevalidateSearchCandidatesRejectsAncestorPathChangesWithoutRevisionChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, Node)
	}{
		{name: "rename", mutate: func(t *testing.T, s *Store, parent Node) {
			t.Helper()
			_, _, err := s.Move(t.Context(), parent.ID, s.RootID(), "renamed", parent.Revision)
			require.NoError(t, err)
		}},
		{name: "move", mutate: func(t *testing.T, s *Store, parent Node) {
			t.Helper()
			destination, err := s.Mkdir(t.Context(), s.RootID(), "destination")
			require.NoError(t, err)
			_, _, err = s.Move(t.Context(), parent.ID, destination.ID, parent.Name, parent.Revision)
			require.NoError(t, err)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			parent, err := s.Mkdir(t.Context(), s.RootID(), "docs")
			require.NoError(t, err)
			child, err := s.CreateFile(t.Context(), parent.ID, "report.txt", fakeHash(test.name), 6, "text/plain")
			require.NoError(t, err)
			parent, err = s.NodeByID(t.Context(), parent.ID)
			require.NoError(t, err)
			candidate := searchCandidateIdentity(t, s, child, SearchEvidenceIdentity{Kind: "node_name"})
			unchanged, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate},
				SearchOptions{}, "", "")
			require.NoError(t, err)
			require.Len(t, unchanged.Candidates, 1)

			test.mutate(t, s, parent)
			currentChild, err := s.NodeByID(t.Context(), child.ID)
			require.NoError(t, err)
			require.Equal(t, child.Revision, currentChild.Revision,
				"ancestor path changes must not rely on descendant revision changes")
			stale, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate},
				SearchOptions{}, "", "")
			require.NoError(t, err)
			assert.Empty(t, stale.Candidates)
		})
	}
}

func TestRevalidateSearchCandidatesAppliesCurrentScopeAndBlobEvidence(t *testing.T) {
	s := newTestStore(t)
	text, err := s.CreateFile(t.Context(), s.RootID(), "text.txt", testSHA256([]byte("text")), 4, "text/plain")
	require.NoError(t, err)
	pdf, err := s.CreateFile(t.Context(), s.RootID(), "paper.pdf", testSHA256([]byte("pdf")), 3, "application/pdf")
	require.NoError(t, err)
	requested := []SearchCandidateIdentity{
		searchCandidateIdentity(t, s, text, SearchEvidenceIdentity{Kind: "content_blob", BlobHash: text.BlobHash}),
		searchCandidateIdentity(t, s, pdf, SearchEvidenceIdentity{Kind: "content_blob", BlobHash: pdf.BlobHash}),
	}

	revalidation, err := s.RevalidateSearchCandidates(t.Context(), requested,
		SearchOptions{MIMEType: "application/pdf"}, "", "")
	require.NoError(t, err)
	require.Len(t, revalidation.Candidates, 1)
	assert.Equal(t, pdf.ID, revalidation.Candidates[0].NodeID)
	requested[1].Evidence[0].BlobHash = testSHA256([]byte("stale-pdf"))
	revalidation, err = s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, "", "")
	require.NoError(t, err)
	require.Len(t, revalidation.Candidates, 1)
	assert.Equal(t, text.ID, revalidation.Candidates[0].NodeID)
}

func TestRevalidateSearchCandidatesEnforcesSourceFenceAcrossReplacement(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.txt", fakeHash("source-fence-first"), 5, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.txt", fakeHash("source-fence-second"), 6, "text/plain")
	require.NoError(t, err)
	requested := []SearchCandidateIdentity{
		searchCandidateIdentity(t, s, first, SearchEvidenceIdentity{Kind: "node_name"}),
		searchCandidateIdentity(t, s, second, SearchEvidenceIdentity{Kind: "node_name"}),
	}

	unrestricted, err := s.RevalidateSearchCandidates(
		t.Context(), requested, SearchOptions{}, "", "")
	require.NoError(t, err)
	require.Equal(t, requested, unrestricted.Candidates)
	_, err = s.RevalidateSearchCandidates(t.Context(), requested,
		SearchOptions{ContentVersionIDs: []string{"not-a-content-version"}}, "", "")
	require.ErrorContains(t, err, "invalid content version ID")
	oldVersionID := second.CurrentVersionID
	restricted, err := s.RevalidateSearchCandidates(t.Context(), requested,
		SearchOptions{ContentVersionIDs: []string{oldVersionID}}, "", "")
	require.NoError(t, err)
	require.Equal(t, requested[1:], restricted.Candidates)

	replacementHash := fakeHash("source-fence-replacement")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, replacementHash, 7)
	}))
	replacement, _, err := s.ReplaceContent(t.Context(), second.ID, second.Revision,
		replacementHash, 7, "text/plain")
	require.NoError(t, err)
	replacementCandidate := searchCandidateIdentity(
		t, s, replacement, SearchEvidenceIdentity{Kind: "node_name"})
	restricted, err = s.RevalidateSearchCandidates(t.Context(),
		[]SearchCandidateIdentity{replacementCandidate},
		SearchOptions{ContentVersionIDs: []string{oldVersionID}}, "", "")
	require.NoError(t, err)
	require.Empty(t, restricted.Candidates,
		"the fenced old version must not authorize its current replacement")
	restricted, err = s.RevalidateSearchCandidates(t.Context(),
		[]SearchCandidateIdentity{replacementCandidate},
		SearchOptions{ContentVersionIDs: []string{replacement.CurrentVersionID}}, "", "")
	require.NoError(t, err)
	require.Equal(t, []SearchCandidateIdentity{replacementCandidate}, restricted.Candidates)
}

func TestRevalidateSearchCandidatesRejectsRemovedScopeReplacementAndTrash(t *testing.T) {
	t.Run("tag scope removal", func(t *testing.T) {
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "tagged.txt", fakeHash("tagged"), 6, "text/plain")
		require.NoError(t, err)
		tag, err := s.CreateTag(t.Context(), "selected")
		require.NoError(t, err)
		_, err = s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		node, err = s.NodeByID(t.Context(), node.ID)
		require.NoError(t, err)
		candidate := searchCandidateIdentity(t, s, node, SearchEvidenceIdentity{Kind: "node_name"})
		_, err = s.UnassignTag(t.Context(), tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		result, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate},
			SearchOptions{TagID: tag.ID}, "", "")
		require.NoError(t, err)
		assert.Empty(t, result.Candidates)
	})

	t.Run("content replacement", func(t *testing.T) {
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "replace.txt", testSHA256([]byte("old")), 3, "text/plain")
		require.NoError(t, err)
		candidate := searchCandidateIdentity(t, s, node,
			SearchEvidenceIdentity{Kind: "content_blob", BlobHash: node.BlobHash})
		newHash := testSHA256([]byte("new"))
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			return s.EnsureBlobTx(tx, newHash, 3)
		}))
		_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, newHash, 3, "text/plain")
		require.NoError(t, err)
		result, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate}, SearchOptions{}, "", "")
		require.NoError(t, err)
		assert.Empty(t, result.Candidates)
	})

	t.Run("trash", func(t *testing.T) {
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "trash.txt", fakeHash("trash"), 5, "text/plain")
		require.NoError(t, err)
		candidate := searchCandidateIdentity(t, s, node, SearchEvidenceIdentity{Kind: "node_name"})
		_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
		result, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate}, SearchOptions{}, "", "")
		require.NoError(t, err)
		assert.Empty(t, result.Candidates)
	})
}

func TestRevalidateSearchCandidatesRequiresCurrentActiveRenditionEvidence(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, testSHA256([]byte("first-rendition")), "first evidence")
	require.NoError(t, s.StageRenditionBuild(t.Context(), firstBuild))
	firstGeneration, err := s.StageLexicalGeneration(t.Context(), testSHA256([]byte("first-lexical-generation")))
	require.NoError(t, err)
	firstAttachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: firstBuild.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: embeddingCatalogTime}, firstGeneration.ID))
	node := nodeForVersion(t, s, versions[0])
	requested := []SearchCandidateIdentity{searchCandidateIdentity(t, s, node,
		SearchEvidenceIdentity{Kind: "rendition_segment", BuildID: firstBuild.ID,
			SegmentID: firstBuild.LexicalSegments[0].ID})}

	revalidation, err := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, "", "")
	require.NoError(t, err)
	require.Len(t, revalidation.Candidates, 1)

	secondBuild := lexicalSearchBuild(s, profile, testSHA256([]byte("second-rendition")), "second evidence")
	const replacementPolicy = `{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":2,"min_count":0,"role":"structured_evidence"}],"version":1}`
	secondBuild.CapturedArtifactPolicy = jsontext.Value(replacementPolicy)
	secondBuild.CapturedArtifactPolicyFingerprint = testSHA256([]byte(replacementPolicy))
	require.NoError(t, s.StageRenditionBuild(t.Context(), secondBuild))
	secondGeneration, err := s.StageLexicalGeneration(t.Context(), testSHA256([]byte("second-lexical-generation")))
	require.NoError(t, err)
	secondAttachment := RenditionAttachmentRecord{ID: catalogAttachmentSecond, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: secondBuild.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), secondAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: secondAttachment.ID, PublishedAt: embeddingCatalogTime}, secondGeneration.ID))
	revalidation, err = s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, "", "")
	require.NoError(t, err)
	assert.Empty(t, revalidation.Candidates)
}

func TestRevalidateSearchCandidatesRejectsStaleSemanticSourceHeadAndSuppression(t *testing.T) {
	for _, mutation := range []string{"head", "suppression"} {
		t.Run(mutation, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint,
				document.EmbeddingInputOriginalFile, "optional", "")
			publishEmbeddingSetForSemanticSearch(t, s, record)
			source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
			require.NoError(t, err)
			node := nodeForVersion(t, s, versionID)
			requested := []SearchCandidateIdentity{searchCandidateIdentity(t, s, node,
				SearchEvidenceIdentity{Kind: "embedding", VectorSpaceID: record.VectorSpace.ID,
					EmbeddingSetID: record.ID, InputGenerationID: record.InputGeneration.ID,
					InputID: record.InputGeneration.Inputs[0].ID, InputKind: record.InputKind,
					SourceManifestChecksum: source.ManifestChecksum})}

			revalidation, err := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{},
				profile.Fingerprint, record.BindingID)
			require.NoError(t, err)
			require.Len(t, revalidation.Candidates, 1)
			require.NotNil(t, revalidation.Coverage)
			assert.Equal(t, 2, revalidation.Coverage.ScopedDocuments)
			assert.Equal(t, 1, revalidation.Coverage.CompleteDocuments)

			if mutation == "head" {
				_, err = s.db.Exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, record.ID)
			} else {
				_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
			}
			require.NoError(t, err)
			_, err = s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{},
				profile.Fingerprint, record.BindingID)
			require.ErrorIs(t, err, ErrVectorIndexSourceStale)
		})
	}
}

func TestRevalidateSearchCandidatesRejectsMismatchedSemanticAuthority(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	node := nodeForVersion(t, s, versionID)
	requested := []SearchCandidateIdentity{searchCandidateIdentity(t, s, node,
		SearchEvidenceIdentity{Kind: "embedding", VectorSpaceID: record.VectorSpace.ID,
			EmbeddingSetID: record.ID, InputGenerationID: record.InputGeneration.ID,
			InputID: record.InputGeneration.Inputs[0].ID, InputKind: record.InputKind,
			SourceManifestChecksum: source.ManifestChecksum})}

	mismatched, err := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, profile.Fingerprint, "required")
	require.NoError(t, err)
	assert.Empty(t, mismatched.Candidates)
	_, err = s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, fakeHash("other-profile"), record.BindingID)
	require.Error(t, err)
}

func TestRevalidateSearchCandidatesRejectsMissingOrUnknownEvidenceFields(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "evidence.txt", fakeHash("evidence"), 8, "text/plain")
	require.NoError(t, err)
	validEmbedding := SearchEvidenceIdentity{Kind: "embedding", VectorSpaceID: fakeHash("space"),
		EmbeddingSetID: fakeHash("set"), InputGenerationID: fakeHash("generation"), InputID: "input",
		InputKind: document.EmbeddingInputOriginalFile, SourceManifestChecksum: fakeHash("source")}
	tests := []struct {
		name     string
		evidence SearchEvidenceIdentity
	}{
		{name: "content blob hash", evidence: SearchEvidenceIdentity{Kind: "content_blob"}},
		{name: "rendition build", evidence: SearchEvidenceIdentity{Kind: "rendition_segment", SegmentID: "segment"}},
		{name: "rendition segment", evidence: SearchEvidenceIdentity{Kind: "rendition_segment", BuildID: "build"}},
		{name: "embedding vector space", evidence: withoutSemanticField(validEmbedding, "space")},
		{name: "embedding set", evidence: withoutSemanticField(validEmbedding, "set")},
		{name: "embedding generation", evidence: withoutSemanticField(validEmbedding, "generation")},
		{name: "embedding input", evidence: withoutSemanticField(validEmbedding, "input")},
		{name: "embedding kind", evidence: withoutSemanticField(validEmbedding, "kind")},
		{name: "embedding source", evidence: withoutSemanticField(validEmbedding, "source")},
		{name: "unknown", evidence: SearchEvidenceIdentity{Kind: "future_evidence"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := searchCandidateIdentity(t, s, node, test.evidence)
			_, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate}, SearchOptions{}, "", "")
			require.ErrorContains(t, err, "evidence")
		})
	}
}

func TestRevalidateSearchCandidatesEnforcesCandidateAndEvidenceBounds(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "bounded.txt", fakeHash("bounded"), 7, "text/plain")
	require.NoError(t, err)
	base := searchCandidateIdentity(t, s, node, SearchEvidenceIdentity{Kind: "node_name"})
	candidates := make([]SearchCandidateIdentity, document.MaxRetrievalCandidateLimit+1)
	for i := range candidates {
		candidates[i] = base
	}
	_, err = s.RevalidateSearchCandidates(t.Context(), candidates, SearchOptions{}, "", "")
	require.ErrorContains(t, err, "retrieval limit")

	base.Evidence = make([]SearchEvidenceIdentity, 33)
	for i := range base.Evidence {
		base.Evidence[i] = SearchEvidenceIdentity{Kind: "node_name"}
	}
	_, err = s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{base}, SearchOptions{}, "", "")
	require.ErrorContains(t, err, "evidence is invalid")
}

func searchCandidateIdentity(t *testing.T, s *Store, node Node,
	evidence ...SearchEvidenceIdentity,
) SearchCandidateIdentity {
	t.Helper()
	path, err := s.Path(t.Context(), node.ID)
	require.NoError(t, err)
	return SearchCandidateIdentity{NodeID: node.ID, NodeRevision: node.Revision,
		ContentVersionID: node.CurrentVersionID, PathChecksum: SearchPathChecksum(path), Evidence: evidence}
}

func nodeForVersion(t *testing.T, s *Store, versionID string) Node {
	t.Helper()
	var nodeID int64
	require.NoError(t, s.db.QueryRow(`SELECT node_id FROM content_versions WHERE version_id=?`, versionID).Scan(&nodeID))
	node, err := s.NodeByID(t.Context(), nodeID)
	require.NoError(t, err)
	return node
}

func withoutSemanticField(value SearchEvidenceIdentity, field string) SearchEvidenceIdentity {
	switch field {
	case "space":
		value.VectorSpaceID = ""
	case "set":
		value.EmbeddingSetID = ""
	case "generation":
		value.InputGenerationID = ""
	case "input":
		value.InputID = ""
	case "kind":
		value.InputKind = ""
	case "source":
		value.SourceManifestChecksum = ""
	}
	return value
}
