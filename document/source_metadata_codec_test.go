package document

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceMetadataRejectsCustodianKeysRegardlessOfVirtualFilename(t *testing.T) {
	for _, name := range []string{
		"Ada Lovelace records.pdf",
		"Grace Hopper - custodian.msg",
		filepath.Join("synthetic", "Records Team", "production.docx"),
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, SourceMetadataCanonicalKeyAllowed("office.custom.custodian"))
			require.False(t, SourceMetadataCanonicalKeyAllowed("office.custom.additional_custodian"))
			require.NotEmpty(t, filepath.Base(name), "the virtual filename is attachment context only")
		})
	}
}
