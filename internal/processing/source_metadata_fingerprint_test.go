package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/internal/emailmime"
)

// Descriptor changes deliberately re-extract every original. Pin the local
// parser bundle separately from the runtime-dependent shared email recipe.
func TestSourceMetadataExtractorDescriptorIsPinned(t *testing.T) {
	assert.Equal(t, "docbank-source-metadata:pdfcpu-info+xmp+pages,"+
		"ooxml-core+custom,emailmime,ical,visual-container+jpeg-tiff-raf-cr3-exif+mp4-created,media-id3:v17",
		sourceMetadataExtractorDescriptor)
}

func TestSourceMetadataExtractorFingerprintIncludesEmailRecipe(t *testing.T) {
	recipe := emailmime.Recipe()
	current := fingerprintSourceMetadataExtractor(sourceMetadataExtractorDescriptor, recipe)
	assert.Equal(t, current, SourceMetadataExtractorFingerprint)
	recipe.GoVersion += "-different"
	assert.NotEqual(t, current, fingerprintSourceMetadataExtractor(sourceMetadataExtractorDescriptor, recipe))
}
