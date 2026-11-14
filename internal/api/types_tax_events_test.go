package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestTaxRecordDetailBalance(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(fromStoreTaxRecordDetail(store.TaxRecordDetail{}, false))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"outstanding_minor":null`)
}
