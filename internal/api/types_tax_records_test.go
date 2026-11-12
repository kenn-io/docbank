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

func TestTaxRecordClearControls(t *testing.T) {
	t.Parallel()
	p := api.PatchTaxRecordRequest{ClearAuthority: true, ClearTaxYear: true, ClearPeriodStart: true, ClearPeriodEnd: true, ClearDueDate: true, ClearLeadDays: true, ClearAmount: true}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	var members map[string]any
	require.NoError(t, json.Unmarshal(raw, &members))
	for _, name := range []string{"authority_party_id", "tax_year", "period_start", "period_end", "due_date", "lead_days", "amount"} {
		require.Contains(t, members, name)
		assert.Nil(t, members[name])
	}
	for _, name := range []string{"clear_authority", "clear_amount", "title"} {
		assert.NotContains(t, members, name)
	}
	for _, raw := range []string{`{"filer_party_id":null}`, `{"currency":null}`, `{"direction":null}`, `{"account_id":"x"}`, `{"kind":"audit"}`, `{"verification":"verified"}`, `{"revision":2}`} {
		var v api.PatchTaxRecordRequest
		assert.Error(t, json.Unmarshal([]byte(raw), &v), raw)
	}
}

func TestTaxRecordCreateNullRoundTrip(t *testing.T) {
	t.Parallel()
	var p api.CreateTaxRecordRequest
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"filing","title":"Example","account_id":null,"authority_party_id":null,"tax_year":null,"period_start":null,"period_end":null,"due_date":null,"lead_days":null,"amount":null}`), &p))
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, field := range []string{"account_id", "authority_party_id", "tax_year", "period_start", "period_end", "due_date", "lead_days", "amount"} {
		require.Contains(t, fields, field)
		assert.Nil(t, fields[field])
	}
}
