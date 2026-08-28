package document

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This test fails if request sizing renders the template before authorization.
func TestModelInputRenderedLengthDoesNotAllocateRenderedText(t *testing.T) {
	contract, err := NewModelInputContract(ModelInputContractConfig{Profile: ModelInputProfileCustom, CompatibilityID: "test-space", Document: ModelInputEncoder{Mode: ModelInputModeText, Template: strings.Repeat("x", 4096-len("{{content}}")) + "{{content}}"}, Query: ModelInputEncoder{Mode: ModelInputModeText, Template: "{{content}}"}})
	require.NoError(t, err)
	content := strings.Repeat("y", 1<<20)
	length, err := modelInputRenderedLength(contract.Document, content)
	require.NoError(t, err)
	assert.Equal(t, int64((1<<20)+4096-len("{{content}}")), length)
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		_, _ = modelInputRenderedLength(contract.Document, content)
	}))
}
