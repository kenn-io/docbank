package uploadcapability

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProofCannotCarryLocalAuthorityThroughJSON(t *testing.T) {
	proof := New(Facts{Checksum: "synthetic"}, new("open"))
	_, err := json.Marshal(proof)
	require.Error(t, err)
	err = json.Unmarshal([]byte(`{}`), &proof)
	require.Error(t, err)
	_, local := proof.Facts()
	require.False(t, local)
}

func TestProofKeepsSelectedPasswordComparable(t *testing.T) {
	for _, password := range []string{"open", ""} {
		candidate := password
		proof := New(Facts{Checksum: "synthetic"}, &candidate)
		candidate = "changed"
		if proof != New(Facts{Checksum: "synthetic"}, &password) {
			t.Fatal("equivalent inspection proofs differ")
		}
		selected, ok := proof.PDFPassword()
		require.True(t, ok)
		require.Equal(t, password, selected)
	}
	_, ok := (Proof{}).PDFPassword()
	require.False(t, ok)
}
