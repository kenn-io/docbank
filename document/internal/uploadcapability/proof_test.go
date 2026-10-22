package uploadcapability

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProofCannotCarryLocalAuthorityThroughJSON(t *testing.T) {
	proof := New(Facts{Checksum: "synthetic"}, []string{"open"})
	_, err := json.Marshal(proof)
	require.Error(t, err)
	err = json.Unmarshal([]byte(`{}`), &proof)
	require.Error(t, err)
	_, local := proof.Facts()
	require.False(t, local)
}

func TestProofKeepsSelectedPasswordPrivateAndComparable(t *testing.T) {
	for _, password := range []string{"open", ""} {
		passwords := []string{password}
		proof := New(Facts{Checksum: "synthetic"}, passwords)
		passwords[0] = "changed"
		if proof != New(Facts{Checksum: "synthetic"}, []string{password}) {
			t.Fatal("equivalent inspection proofs differ")
		}
		selected := proof.PDFPasswords()
		require.Equal(t, []string{password}, selected)
		selected[0] = "changed"
		require.Equal(t, []string{password}, proof.PDFPasswords())
	}
	require.Nil(t, (Proof{}).PDFPasswords())
}
