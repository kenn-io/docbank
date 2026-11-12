package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestTaxIdentifierDisclosure(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{"": "", "a": "****", "abcd": "****", "abcde": "****bcde", "雪月花鳥風": "****月花鳥風"} {
		assert.Equal(t, want, maskTaxIdentifier(input))
	}
}

func TestTaxAccountPatchOmissionAndNull(t *testing.T) {
	t.Parallel()
	date := "2025-01-01"
	a := store.TaxAccount{AccountID: "22222222-2222-4222-8222-222222222222", Revision: 1, FilerPartyID: "11111111-1111-4111-8111-111111111111", Country: "ZZ", TaxKind: "Example", Cadence: "annual", Currency: "USD", Identifier: "private12345", EffectiveStart: &date, EffectiveEnd: &date, LeadDays: 14}
	var create CreateTaxAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"$schema":"https://example.com/schema.json","country":"ZZ"}`), &create))
	var patch PatchTaxAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"$schema":"https://example.com/schema.json","effective_start":null,"lead_days":0}`), &patch))
	in := mergeTaxAccountPatch(a, patch)
	assert.Equal(t, "private12345", in.Identifier)
	assert.Nil(t, in.EffectiveStart)
	assert.Equal(t, &date, in.EffectiveEnd)
	require.NotNil(t, in.LeadDays)
	assert.Zero(t, *in.LeadDays)
	raw, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"effective_start":null`)
	raw, err = json.Marshal(PatchTaxAccountRequest{ClearEffectiveStart: true})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"effective_start":null`)
}
