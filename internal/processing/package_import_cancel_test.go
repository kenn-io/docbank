package processing

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageImportCancellationRemovesOnlyUnreceiptedDocuments(t *testing.T) {
	t.Parallel()
	for _, afterText := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_text_publication_%t", afterText), func(t *testing.T) {
			env := newPackageImportTestEnvOptions(t, 2, false, false, true)
			ctx := t.Context()
			key, err := store.PackageRecordKey("VOL001/DATA.DAT", 2, "DOC-B")
			require.NoError(t, err)
			occurrence := store.PackageOccurrenceID(env.PackageID, key)
			pendingPath := "/" + env.PackageID + "/" + occurrence + "-00-native.txt"
			textPath := "/" + env.PackageID + "/" + occurrence + "-01-supplied_text.txt"
			var cancelled, textStaged bool
			var pending store.Node
			var shared store.Node
			cfg := env.config()
			cfg.Mutate = func(ctx context.Context, fn func() error) error {
				err := fn()
				if err != nil || cancelled {
					return err
				}
				pending, err = env.Catalog.NodeByPath(ctx, pendingPath)
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				require.NoError(t, err)
				if afterText && !textStaged {
					_, err = env.Catalog.NodeByPath(ctx, textPath)
					textStaged = err == nil
					require.True(t, err == nil || errors.Is(err, store.ErrNotFound))
					return nil
				}
				// A separately imported document uses the same bytes as the
				// cancelled record. Its content must survive cancellation.
				run, err := env.Catalog.BeginIngest(ctx, "manual", "synthetic shared document")
				require.NoError(t, err)
				root, err := env.Catalog.NodeByPath(ctx, "/")
				require.NoError(t, err)
				shared, err = env.Catalog.IngestFileExact(ctx, run, root.ID, "shared.txt",
					pending.BlobHash, pending.Size, pending.MimeType, "shared.txt", "")
				require.NoError(t, err)
				_, err = env.Catalog.CancelPackageImportJob(ctx, env.Owner, env.OperationID)
				require.NoError(t, err)
				cancelled = true
				return nil
			}
			worker, err := NewPackageImportWorker(cfg)
			require.NoError(t, err)
			_, err = worker.ProcessOnce(ctx)
			require.Error(t, err)
			require.True(t, cancelled)
			pkg, err := env.Catalog.Package(ctx, env.PackageID)
			require.NoError(t, err)
			require.Equal(t, "cancelled", pkg.State)
			require.Empty(t, pkg.SnapshotID)
			_, err = env.Catalog.PackageImportHead(ctx, env.PackageID, key)
			require.ErrorIs(t, err, store.ErrNotFound)
			for _, name := range []string{pendingPath, textPath} {
				_, err = env.Catalog.NodeByPath(ctx, name)
				require.ErrorIs(t, err, store.ErrNotFound)
			}
			_, err = env.Catalog.ContentVersionByID(ctx, pending.CurrentVersionID)
			require.ErrorIs(t, err, store.ErrNotFound)
			gotShared, err := env.Catalog.NodeByPath(ctx, "/shared.txt")
			require.NoError(t, err)
			require.Equal(t, shared, gotShared)
			stream, _, err := env.Blobs.OpenStreamContext(ctx, shared.BlobHash)
			require.NoError(t, err)
			content, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			require.Equal(t, env.Contents[1], content)
			firstKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
			require.NoError(t, err)
			first, err := env.Catalog.PackageImportHead(ctx, env.PackageID, firstKey)
			require.NoError(t, err)
			var receipt packageImportReceiptData
			require.NoError(t, json.Unmarshal(first.ReceiptJSON, &receipt))
			require.NotNil(t, receipt.Member)
			for _, rep := range receipt.Member.Representations {
				if rep.Status == "available" {
					_, err = env.Catalog.ContentVersionByID(ctx, rep.ContentVersionID)
					require.NoError(t, err, "committed representations must survive")
				}
			}
			require.NoError(t, env.Catalog.ValidateMetadata(ctx))
		})
	}
}
