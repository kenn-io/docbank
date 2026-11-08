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
