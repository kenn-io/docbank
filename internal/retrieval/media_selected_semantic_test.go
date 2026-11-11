package retrieval

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSelectedMediaSemanticExcerpt(t *testing.T) {
	t.Parallel()
	candidate := Candidate{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "content", BuildID: "build"}, Lane: LaneSemantic, Rank: 1, VectorSpaceID: "space", Excerpt: "telescope arrives Friday afternoon", Evidence: []EvidenceReference{{Kind: "embedding", BuildID: "build", InputID: "input", VectorSpaceID: "space", TimeSpan: &MediaTimeSpan{StartMS: 1000, EndMS: 2000}}}}
	report := laneReport(ModeSemantic, ModeSemantic, Coverage{}, []Candidate{candidate}, false)
	require.Equal(t, candidate.Excerpt, report.Results[0].Excerpt)
	fused, _, err := FuseReciprocalRank(nil, []Candidate{candidate}, 1)
	require.NoError(t, err)
	require.Equal(t, candidate.Excerpt, fused[0].Excerpt)
	require.Equal(t, "embedding", fused[0].Evidence[0].Kind)
	require.Empty(t, fused[0].Evidence[0].SegmentID)
}

func FuzzSelectedMediaFusionKeepsBuildIdentity(f *testing.F) {
	f.Add("build-a", "build-b")
	f.Fuzz(func(t *testing.T, first, second string) {
		if first == second || first == "" || second == "" {
			t.Skip()
		}
		one := Candidate{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "content", BuildID: first}, Lane: LaneLexical, Rank: 1, Evidence: []EvidenceReference{{Kind: "rendition_segment", BuildID: first}}}
		two := Candidate{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "content", BuildID: second}, Lane: LaneSemantic, Rank: 1, VectorSpaceID: "space", Evidence: []EvidenceReference{{Kind: "embedding", BuildID: second, VectorSpaceID: "space"}}}
		results, _, err := FuseReciprocalRank([]Candidate{one}, []Candidate{two}, 2)
		require.NoError(t, err)
		require.Len(t, results, 2)
		for _, result := range results {
			require.Len(t, result.Evidence, 1)
			require.Equal(t, result.Document.BuildID, result.Evidence[0].BuildID)
		}
		require.Less(t, results[0].Document.BuildID, results[1].Document.BuildID)
	})
}
