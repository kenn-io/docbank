package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRerankingPlanFingerprintBindsProviderPolicy(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Flow: []FlowHop{{Capability: "reranking", ProviderID: "zeroentropy", TrustBoundary: "hosted_provider",
			InputClasses: []string{"query_text_and_excerpt"}, RuntimeDisclosure: RuntimeDisclosure{
				ImmediateProcessor: "docbank-zeroentropy-rerank", UltimateProcessor: "zeroentropy",
				Endpoint: "https://api.zeroentropy.dev", Deployment: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Model: "zerank-2", ModelRevision: "v1", MetadataClasses: []string{"candidate_excerpt"},
			}}},
		DisclosedClasses: []string{"query_text_and_excerpt"}, ConsentRequired: true}
	base, err := planFingerprint(plan)
	require.NoError(t, err)
	plan.Flow[0].RuntimeDisclosure.Deployment = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	changed, err := planFingerprint(plan)
	require.NoError(t, err)
	require.NotEqual(t, base, changed)
}
