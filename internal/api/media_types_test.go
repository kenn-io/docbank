package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaReferenceBodyDecodesWithoutEnteringReceipt(t *testing.T) {
	t.Parallel()
	var in MediaReferenceBody
	err := json.Unmarshal([]byte(`{"operation_id":"00000000-0000-4000-8000-000000000001","reference_url":"https://cap.so/s/private-id?sig=SECRET","canonical_url":"https://recordings.invalid/share/call","acquire":false}`), &in)
	require.NoError(t, err)
	require.Contains(t, in.ReferenceURL, "SECRET")
	require.Equal(t, "https://recordings.invalid/share/call", in.CanonicalURL)
	out := MediaReceipt{OperationID: in.OperationID, Outcome: "access_required"}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET")
	require.NotContains(t, string(raw), "private-id")
}
