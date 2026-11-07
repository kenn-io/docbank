//go:build !darwin || arm64

package vision

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestUnderstandingRejectsMissingSemanticLayerProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		missing := hegel.Draw(ht, hegel.SampledFrom([]string{"visible_text", "literal_description", "possible_meaning", "uncertainty"}))
		fields := map[string]string{"visible_text": text, "literal_description": text, "possible_meaning": text, "uncertainty": text}
		delete(fields, missing)
		raw, err := json.Marshal(fields)
		require.NoError(ht, err)
		_, err = parseUnderstanding(raw)
		require.Error(ht, err)
	}, hegel.WithDatabase(t.TempDir()))
}
