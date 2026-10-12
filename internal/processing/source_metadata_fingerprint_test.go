package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/internal/emailmime"
)

func TestSourceMetadataExtractorFingerprintIncludesEmailRecipe(t *testing.T) {
	t.Parallel()
	recipe := emailmime.Recipe()
	current := fingerprintSourceMetadataExtractor(sourceMetadataExtractorDescriptor, recipe)
	assert.Equal(t, current, SourceMetadataExtractorFingerprint)
	recipe.GoVersion += "-different"
	assert.NotEqual(t, current, fingerprintSourceMetadataExtractor(sourceMetadataExtractorDescriptor, recipe))
}
