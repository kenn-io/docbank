package api

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

func TestTaxPatchOmissionNullAndMoney(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code, want string
		minor      int64
	}{
		{"USD", "1234.00", 123400}, {"JPY", "1234", 1234}, {"BHD", "1.234", 1234}, {"USD", "92233720368547758.07", math.MaxInt64},
	} {
		t.Run(tc.code+tc.want, func(t *testing.T) {
			r := store.TaxRecord{RecordValues: taxrecord.RecordValues{Currency: tc.code, AmountMinor: &tc.minor, Direction: "payable", Title: "Example"}}
			merged, err := mergeTaxRecordPatch(r, PatchTaxRecordRequest{Note: new("example")})
			require.NoError(t, err)
			require.NotNil(t, merged.Amount)
			assert.Equal(t, tc.want, *merged.Amount)
		})
	}
	value := int64(0)
	r := store.TaxRecord{RecordValues: taxrecord.RecordValues{Currency: "USD", AmountMinor: &value, Direction: "payable"}}
	_, err := mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("JPY")})
	assert.Error(t, err)
	merged, err := mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("JPY"), Amount: new("1234")})
	require.NoError(t, err)
	assert.Equal(t, "1234", *merged.Amount)
	merged, err = mergeTaxRecordPatch(r, PatchTaxRecordRequest{Currency: new("usd")})
	require.NoError(t, err)
	assert.Equal(t, "0.00", *merged.Amount)
	merged, err = mergeTaxRecordPatch(r, PatchTaxRecordRequest{ClearAmount: true, Direction: new("unknown")})
	require.NoError(t, err)
	assert.Nil(t, merged.Amount)
}
