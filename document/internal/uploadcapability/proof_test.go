package uploadcapability

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProofCannotCarryLocalAuthorityThroughJSON(t *testing.T) {
	proof := New(Facts{Checksum: "synthetic"})
	_, err := json.Marshal(proof)
	require.Error(t, err)
	err = json.Unmarshal([]byte(`{}`), &proof)
	require.Error(t, err)
	_, local := proof.Facts()
	require.False(t, local)
}
