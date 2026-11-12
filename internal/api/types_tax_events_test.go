package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

func TestTaxRecordDetailBalance(t *testing.T) {
	d := store.TaxRecordDetail{Balance: taxrecord.Balance{OutstandingMinor: new(int64(113400)), Currency: "USD", OtherCurrencyEvents: 2}}
	got := fromStoreTaxRecordDetail(d, false)
	assert.Equal(t, new(int64(113400)), got.Balance.OutstandingMinor)
	assert.Equal(t, int64(2), got.Balance.OtherCurrencyEvents)
	raw, err := json.Marshal(fromStoreTaxRecordDetail(store.TaxRecordDetail{}, false))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"outstanding_minor":null`)
}

func TestTaxCashRequestMembers(t *testing.T) {
	var req AddTaxEventRequest
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"payment","occurred_on":"2026-01-01","amount":"12.34","currency":"USD"}`), &req))
	assert.Equal(t, new("12.34"), req.Amount)
	assert.Equal(t, "USD", req.Currency)
	require.Error(t, json.Unmarshal([]byte(`{"kind":"payment","currency":null}`), &req))
}
