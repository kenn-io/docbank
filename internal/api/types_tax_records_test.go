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
	create, err := json.Marshal(api.CreateTaxRecordRequest{Kind: "filing", Title: "Example", ClearAuthority: true})
	require.NoError(t, err)
	assert.Contains(t, string(create), `"authority_party_id":null`)
}
