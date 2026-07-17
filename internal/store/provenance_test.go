package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProvenanceIdentityGolden(t *testing.T) {
	originalMTime := "2026-01-02T03:04:05.12Z"
	identity, err := provenanceIdentity(metadataProvenance{
		Type:          metadataProvenanceType,
		NodeID:        42,
		IngestID:      "00112233-4455-4677-8899-aabbccddeeff",
		OriginalPath:  "/source/report.txt",
		OriginalMTime: &originalMTime,
	})
	require.NoError(t, err)
	require.Equal(t, "2d6ccdf760691482e62a95cb57623282c919aaba7a69808ff6da2e2c7af95516", identity)
}
