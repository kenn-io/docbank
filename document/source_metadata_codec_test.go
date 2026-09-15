package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceMetadataRejectsCustodianKeys(t *testing.T) {
	for _, key := range []string{"office.custom.custodian", "office.custom.additional_custodian"} {
		t.Run(key, func(t *testing.T) {
			require.False(t, SourceMetadataCanonicalKeyAllowed(key))
		})
	}
	require.True(t, SourceMetadataCanonicalKeyAllowed("office.custom.department"))
}
