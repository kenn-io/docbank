package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCapabilityVocabulariesAreClosed(t *testing.T) {
	assert.Equal(t, []CapabilityKey{"detect", "retain", "metadata", "expand", "text", "pages", "transcript"},
		AllCapabilityKeys())
	assert.Equal(t, []CapabilityState{"qualified", "provider_required", "unqualified", "unsupported", "not_applicable"},
		AllCapabilityStates())
	assert.True(t, ValidCapabilityKey(CapabilityExpand))
	assert.False(t, ValidCapabilityKey("ocr"))
	assert.True(t, ValidCapabilityState(CapabilityProviderRequired))
	assert.False(t, ValidCapabilityState("maybe"))
	assert.Equal(t, "format-coverage/v1", FormatCoverageContractV1)
}

func TestCapabilityVocabulariesReturnDefensiveCopies(t *testing.T) {
	keys := AllCapabilityKeys()
	states := AllCapabilityStates()
	keys[0] = "forged"
	states[0] = "forged"
	assert.Equal(t, CapabilityDetect, AllCapabilityKeys()[0])
	assert.Equal(t, CapabilityQualified, AllCapabilityStates()[0])
}
