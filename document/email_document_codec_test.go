package document_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"strings"
	"testing"
)

func TestEmailDocumentJSONRejectsUnboundedAndAmbiguousInput(t *testing.T) {
	for _, raw := range []string{`null`, `{"operation_id":"a","operation_id":"b"}`, `{"unknown":1}`, `{"destination_id":9007199254740992}`, `{"destination_id":1.0}`, `{"reuse":[` + strings.Repeat(`{},`, 1000) + `{}]}`, strings.Repeat(" ", 2<<20) + `{}`} {
		var request document.EmailDocumentPublicationRequest
		require.Error(t, document.UnmarshalEmailDocumentJSON([]byte(raw), &request))
	}
	var request document.EmailDocumentPublicationRequest
	require.NoError(t, document.UnmarshalEmailDocumentJSON([]byte(`{"operation_id":"synthetic","reuse":[]}`), &request))
	require.Equal(t, "synthetic", request.OperationID)
}
