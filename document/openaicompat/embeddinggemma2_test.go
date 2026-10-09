package openaicompat

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

// Every literal byte of either role is vector-space authority, even whitespace.
// This property catches trimming or omission of an affix from canonical identity.
func FuzzEmbeddingGemma2RoleIdentity(f *testing.F) {
	for _, affix := range []string{"", " ", "\n", "title: none | text: ", "task: search result | query: ", "\x00", "{{content}}", "世界", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		f.Add(affix, false)
		f.Add(affix, true)
	}
	f.Fuzz(func(t *testing.T, affix string, query bool) {
		const slot = "{{content}}"
		config := document.ModelInputContractConfig{
			Profile: document.ModelInputProfileCustom, CompatibilityID: "embeddinggemma2/search/native768/v1",
			Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "title: none | text: " + slot},
			Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "task: search result | query: " + slot},
		}
		base := testProfile(t, modelInput(t, config))
		base.Descriptor.Dimension = 768
		base.Descriptor = descriptorFor(t, base)
		encoder := &config.Document
		if query {
			encoder = &config.Query
		}
		encoder.Template = affix + encoder.Template
		contract, err := document.NewModelInputContract(config)
		// These are the documented template admission rules, independent of hashing.
		invalid := !utf8.ValidString(encoder.Template) || len(encoder.Template) > 4096 || strings.Count(encoder.Template, slot) != 1
		if invalid {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		changed := base
		changed.ModelInput, changed.Descriptor.ModelInput = contract, contract
		changed.Descriptor = descriptorFor(t, changed)
		if affix == "" {
			assert.Equal(t, base.Descriptor, changed.Descriptor)
		} else {
			assert.NotEqual(t, base.Descriptor.PolicyFingerprint, changed.Descriptor.PolicyFingerprint)
			assert.NotEqual(t, base.Descriptor.Fingerprint, changed.Descriptor.Fingerprint)
		}
		assert.Equal(t, changed.Descriptor, descriptorFor(t, changed), "canonical construction must be stable")
	})
}
