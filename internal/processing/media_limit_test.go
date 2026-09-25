package processing

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
)

func TestMediaMaxBytesAllowsSuppliedMedia(t *testing.T) {
	t.Parallel()
	for _, maximum := range []int64{1 << 30, (1 << 30) + 1} {
		t.Run(strconv.FormatInt(maximum, 10), func(t *testing.T) {
			fixture := newPublicationFixture(t)
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.WAV()))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "call.wav",
				written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
			require.NoError(t, err)
			service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
				Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaMaxBytes: maximum,
			})
			if maximum > 1<<30 {
				require.ErrorContains(t, err, "media byte limit")
				return
			}
			require.NoError(t, err)
			receipt, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000703", Filename: "call.wav",
				MediaType: "audio/wav", SHA256: written.Hash, ByteLength: written.Size,
				ExistingContentVersionID: node.CurrentVersionID,
				Occurrence:               MediaOccurrenceInput{Ref: "message", Revision: "1"},
			})
			require.NoError(t, err)
			require.Equal(t, node.CurrentVersionID, receipt.ContentVersionID)
		})
	}
}
