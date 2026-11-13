package retrieval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
		require.ElementsMatch(t, []string{first, second}, []string{results[0].Document.BuildID, results[1].Document.BuildID})
		for _, result := range results {
			require.Len(t, result.Evidence, 1)
			require.Equal(t, result.Document.BuildID, result.Evidence[0].BuildID)
		}
	})
}
