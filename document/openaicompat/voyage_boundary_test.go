package openaicompat

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestOpenAICompatibleStillRejectsVoyageTextMember(t *testing.T) {
	contract := modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	profile := testProfile(t, contract)
	response := []byte(strings.Replace(string(successIndexedResponse), `"index":`, `"text":"synthetic passage","index":`, 1))
	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, response), nil
	}))
	_, err := document.ExecuteEmbedding(t.Context(), client, testInputs(), testAuthorization(profile.Descriptor))
	require.ErrorIs(t, err, ErrMalformedResponse)
}
