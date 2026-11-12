package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestTaxPatchOmissionNullAndMoney(t *testing.T) {
	t.Parallel()
	value := int64(0)
	r := store.TaxRecord{Currency: "USD", AmountMinor: &value, Direction: "payable"}
	_, err := mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("JPY")})
	require.Error(t, err)
	merged, err := mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("JPY"), Amount: new("1234")})
	require.NoError(t, err)
	assert.Equal(t, "1234", *merged.Amount)
	merged, err = mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("usd")})
	require.NoError(t, err)
	assert.Equal(t, "0.00", *merged.Amount)
}
