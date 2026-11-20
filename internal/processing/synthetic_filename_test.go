package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Capability inspection accepts these containers only under these extensions,
// so a withheld source name must not depend on the host MIME database, which
// may not list them.
func TestSyntheticFilenameUsesInspectedContainerExtensions(t *testing.T) {
	t.Parallel()
	for mediaType, want := range map[string]string{
		"application/epub+zip":                                                      "document.epub",
		"application/vnd.oasis.opendocument.spreadsheet":                            "document.ods",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": "document.pptx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "document.xlsx",
	} {
		require.Equal(t, want, syntheticFilename("book.bin", mediaType, false), mediaType)
	}
}
