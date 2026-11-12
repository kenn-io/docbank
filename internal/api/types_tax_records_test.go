package api_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxRecordDTOJSONPresence(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"$schema":"https://example.com/schema"}`, `{"title":null}`, `{"state":"filed"}`, `{"unknown":"value"}`} {
		var patch api.PatchTaxRecordRequest
		assert.Error(t, json.Unmarshal([]byte(raw), &patch), raw)
	}
	var patch api.PatchTaxRecordRequest
	require.NoError(t, json.Unmarshal([]byte(`{"amount":null,"tax_year":null,"note":"example"}`), &patch))
	data, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"amount":null`)
	assert.Contains(t, string(data), `"tax_year":null`)
	assert.NotContains(t, string(data), `"due_date"`)
	var create api.CreateTaxRecordRequest
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"filing","title":"Example","authority_party_id":null}`), &create))
	data, err = json.Marshal(create)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"authority_party_id":null`)
}
