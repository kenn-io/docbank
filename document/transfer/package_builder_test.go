package transfer_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/transfer"
	"go.kenn.io/docbank/document/transfer/transfertest"
)

func TestSyntheticPackageBuilderProducesReadableDirectoryAndZip(t *testing.T) {
	line := transfer.SourceLineV1{
		RecordType: transfer.RecordTypeSource,
		SourceRef:  "source-1",
		SourceType: transfer.SourceTypeSlack,
		Route:      "slack",
		Identifier: "workspace-1",
	}
	for _, zipped := range []bool{false, true} {
		path := transfertest.Build(t, transfertest.Spec{
			Manifest: transfer.ManifestV1{Archive: transfer.ArchiveV1{ArchiveID: "archive-1"}},
			Lines:    []any{line},
			Blobs:    map[string][]byte{},
			Zip:      zipped,
		})
		var reader transfer.PackageReader
		var err error
		if zipped {
			file, openErr := os.Open(path)
			require.NoError(t, openErr)
			info, statErr := file.Stat()
			require.NoError(t, statErr)
			reader, err = transfer.OpenZip(t.Context(), file, info.Size())
		} else {
			reader, err = transfer.OpenDirectory(t.Context(), path)
		}
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		require.Equal(t, int64(1), reader.Manifest().Counts.Source)
		require.NotEqual(t, bytes.Repeat([]byte{'0'}, 64), []byte(reader.Manifest().RecordsSHA256))
	}
}
