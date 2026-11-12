package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

func TestTaxIdentifierDisclosure(t *testing.T) {
	for input, want := range map[string]string{"": "", "a": "****", "abcd": "****", "abcde": "****bcde", "雪月花鳥風": "****月花鳥風"} {
		assert.Equal(t, want, maskTaxIdentifier(input))
	}
}

func TestTaxAccountPatchOmissionAndNull(t *testing.T) {
	date := "2025-01-01"
	a := store.TaxAccount{AccountValues: taxrecord.AccountValues{Identifier: "private12345", EffectiveStart: &date, EffectiveEnd: &date, LeadDays: 14}}
	var patch PatchTaxAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"effective_start":null,"lead_days":0}`), &patch))
	in, err := mergeTaxAccountPatch(a, patch)
	require.NoError(t, err)
	assert.Equal(t, "private12345", in.Identifier)
	assert.Nil(t, in.EffectiveStart)
	assert.Equal(t, &date, in.EffectiveEnd)
	require.NotNil(t, in.LeadDays)
	assert.Zero(t, *in.LeadDays)
	raw, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"effective_start":null`)
	empty := PatchTaxAccountRequest{Identifier: new(""), LeadDays: new(int64(0))}
	raw, err = json.Marshal(empty)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"identifier":""`)
	assert.Contains(t, string(raw), `"lead_days":0`)
	for _, body := range []string{`{}`, `{"identifier":null}`, `{"country":"ZZ"}`, `{"unknown_private":"secret"}`, `null`, `[]`} {
		var invalid PatchTaxAccountRequest
		assert.Error(t, json.Unmarshal([]byte(body), &invalid), body)
	}
}
