package mailbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func mailboxFixture(t *testing.T, drivers ...docsqlite.Driver) *Service {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"), drivers...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	b, err := blob.New(store.NewPackCatalog(s), filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return &Service{Store: s, Blobs: b, Spool: dir}
}
func TestContainerVerifiesPayloadBeforeSealAndRandomAccess(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	raw := []byte("synthetic mailbox source")
	req := store.MailboxContainerRequest{ID: "container", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
	_, err := f.Store.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	err = f.UploadChunk(ctx, "one", req.ID, 0, req.SHA256, req.Size, bytes.NewReader([]byte("wrong")))
	require.Error(t, err)
	c, err := f.Store.MailboxContainer(ctx, "one", req.ID)
	require.NoError(t, err)
	require.Empty(t, c.Chunks)
	require.NoError(t, f.UploadChunk(ctx, "one", req.ID, 0, req.SHA256, req.Size, bytes.NewReader(raw)))
	c, err = f.Seal(ctx, "one", req.ID)
	require.NoError(t, err)
	r, err := f.ReaderAt(ctx, c)
	require.NoError(t, err)
	small := make([]byte, 4)
	n, err := r.ReadAt(small, 10)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, "mail", string(small))
	n, err = r.ReadAt(small, req.Size-2)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 2, n)
	_, err = r.ReadAt(small, -1)
	require.Error(t, err)
	c.SHA256 = hashBytes([]byte("changed"))
	_, err = f.ReaderAt(ctx, c)
	require.Error(t, err)
}

func TestAbandonedMailboxChunksAreCollected(t *testing.T) {
	for _, mode := range []string{"mismatch", "abort", "expire"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := mailboxFixture(t)
				ctx := t.Context()
				raw := []byte("synthetic abandoned chunk")
				c := store.MailboxContainerRequest{ID: "container", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
				_, err := f.Store.BeginMailboxContainer(ctx, c)
				require.NoError(t, err)
				declared := c.SHA256
				if mode == "mismatch" {
					declared = hashBytes([]byte("different declaration"))
				}
				err = f.UploadChunk(ctx, c.Owner, c.ID, 0, declared, c.Size, bytes.NewReader(raw))
				if mode == "mismatch" {
					require.ErrorIs(t, err, store.ErrMailboxConflict)
				} else {
					require.NoError(t, err)
					if mode == "abort" {
						require.NoError(t, f.Store.AbortMailboxContainer(ctx, c.Owner, c.ID))
					} else {
						time.Sleep(25 * time.Hour)
						require.NoError(t, f.Store.CleanupMailboxContainers(ctx))
					}
				}
				var report maintenance.GCReport
				require.NoError(t, f.Blobs.WithMaintenance(ctx, func() error {
					report, err = maintenance.GarbageCollect(ctx, f.Store, f.Blobs, maintenance.GCOptions{})
					return err
				}))
				require.Equal(t, 1, report.RemovedBlobs)
				physical, err := f.Blobs.ListDetailed()
				require.NoError(t, err)
				require.NotContains(t, physical, c.SHA256)
			})
		})
	}
}

func TestAbortedMailboxUploadPreservesSharedBlob(t *testing.T) {
	for _, retainedBy := range []string{"upload", "rendition"} {
		t.Run(retainedBy, func(t *testing.T) {
			f := mailboxFixture(t)
			ctx := t.Context()
			raw := []byte("synthetic shared chunk")
			c := store.MailboxContainerRequest{ID: "retained", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
			if retainedBy == "upload" {
				_, err := f.Store.BeginMailboxContainer(ctx, c)
				require.NoError(t, err)
				require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
			} else {
				require.NoError(t, f.Blobs.WithMutation(ctx, func() error {
					written, err := f.Blobs.WriteDetailedContext(ctx, bytes.NewReader(raw))
					if err != nil {
						return err
					}
					encoding, err := written.EncodingName()
					if err != nil {
						return err
					}
					return f.Store.RecordRenditionBlob(ctx, written.Hash, written.Size, store.BlobPhysical{
						Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, Created: written.Created,
					})
				}))
			}
			c.ID = "aborted"
			_, err := f.Store.BeginMailboxContainer(ctx, c)
			require.NoError(t, err)
			require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
			require.NoError(t, f.Store.AbortMailboxContainer(ctx, c.Owner, c.ID))
			var report maintenance.GCReport
			require.NoError(t, f.Blobs.WithMaintenance(ctx, func() error {
				report, err = maintenance.GarbageCollect(ctx, f.Store, f.Blobs, maintenance.GCOptions{})
				return err
			}))
			require.Zero(t, report.RemovedBlobs)
			reader, _, err := f.Blobs.OpenStreamContext(ctx, c.SHA256)
			require.NoError(t, err)
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, raw, got)
		})
	}
}

func TestFailedMailboxTransferIsCollected(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	raw := []byte("Subject: Synthetic failure\r\n\r\nSynthetic body.\r\n")
	_, err := f.Transfer(ctx, "one", store.MailboxTransferRequest{
		ArchiveID: "unregistered", Reference: "message-1", SHA256: hashBytes(raw), Size: int64(len(raw)),
		Settings: "synthetic", DestinationID: f.Store.RootID(), Name: "message.eml",
	}, bytes.NewReader(raw))
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.Blobs.WithMaintenance(ctx, func() error {
		_, err := maintenance.GarbageCollect(ctx, f.Store, f.Blobs, maintenance.GCOptions{})
		return err
	}))
	physical, err := f.Blobs.ListDetailed()
	require.NoError(t, err)
	require.Empty(t, physical)
}
