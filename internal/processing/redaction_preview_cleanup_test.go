package processing

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreviewCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	runtime := redactiontest.PageRuntime(t)
	for _, phase := range []string{"creation", "last reader"} {
		t.Run(phase, func(t *testing.T) {
			f := newPreparationFixture(t, "SECRET")
			prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
			require.NoError(t, err)
			masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
				Kind: "page", MapSHA256: prep.Map.SHA256, Pages: []int{1},
			}}}
			review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
			require.NoError(t, err)
			request := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
			var directory string
			t.Cleanup(func() {
				if directory != "" {
					require.NoError(t, os.Chmod(directory, 0o700))
				}
				require.NoError(t, f.service.DiscardRedactionPreviews("alice"))
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			preview, err := f.service.CreateRedactionPreview(ctx, "alice", request, runtime, func(publish func() error) error {
				directories, err := filepath.Glob(filepath.Join(f.service.spoolDirectory, "native-pdf-*"))
				require.NoError(t, err)
				require.Len(t, directories, 1)
				directory = directories[0]
				if phase == "creation" {
					require.NoError(t, os.Chmod(directory, 0o500))
					cancel() // Cancel before the publication fence, with real staged output.
				}
				return publish()
			})
			if phase == "creation" {
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, err, fs.ErrPermission)
			} else {
				require.NoError(t, err)
				reader, err := f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "bundle", 0)
				require.NoError(t, err)
				defer func() { _ = reader.Close() }()
				require.NoError(t, os.Chmod(directory, 0o500))
				require.NoError(t, f.service.DiscardRedactionPreviews("alice"))
				require.ErrorIs(t, reader.Close(), fs.ErrPermission)
			}
			// Failed cleanup remains retryable and blocks another preview's disk allocation.
			require.ErrorIs(t, f.service.DiscardRedactionPreviews("alice"), fs.ErrPermission)
			_, err = f.service.CreateRedactionPreview(t.Context(), "alice", request, runtime, nil)
			require.ErrorIs(t, err, fs.ErrPermission)
			files, err := os.ReadDir(directory)
			require.NoError(t, err)
			require.NotEmpty(t, files)
			// Repeated cleanup failures must not keep shutdown waiting after work finishes.
			shutdown, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			require.NoError(t, f.service.Shutdown(shutdown))
			require.NoError(t, os.Chmod(directory, 0o700))
			require.NoError(t, f.service.DiscardRedactionPreviews("alice"))
			directory = ""
			files, err = os.ReadDir(f.service.spoolDirectory)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}
