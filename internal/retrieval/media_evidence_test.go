package retrieval

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestMediaTimeSpanPreservesZeroAndRejectsInventedTiming(t *testing.T) {
	span, err := mediaTimeSpan(document.EvidenceLocatorV1{
		Kind:        document.EvidenceLocatorSegment,
		IndexOrigin: document.EvidenceIndexOriginZero,
		Start:       0,
		End:         1000,
	})
	require.NoError(t, err)
	raw, err := json.Marshal(span)
	require.NoError(t, err)
	require.JSONEq(t, `{"start_ms":0,"end_ms":1000}`, string(raw))

	span, err = mediaTimeSpan(document.EvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric})
	require.NoError(t, err)
	require.Nil(t, span)
}
