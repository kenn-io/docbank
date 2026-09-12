package store

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An authenticated upload cannot replace accepted bytes, evade its absolute
// lifetime or seal a source with missing ordered chunks.
func TestMailboxContainerSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := MailboxContainerRequest{ID: "container-one", Owner: "owner-one", SHA256: fakeHash("aa"), Size: 3, Format: "mbox"}
	c, err := s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "uploading", c.State)
	_, err = s.MailboxContainer(ctx, "other-owner", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.SealMailboxContainer(ctx, req.Owner, req.ID, req.SHA256, 3)
	require.Error(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, req.SHA256, 3, BlobPhysical{Encoding: "raw", StoredBytes: 3, PackEligible: true, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, req.Owner, req.ID, MailboxChunk{Index: 0, SHA256: req.SHA256, Size: 3}))
	require.NoError(t, s.PutMailboxChunk(ctx, req.Owner, req.ID, MailboxChunk{Index: 0, SHA256: req.SHA256, Size: 3}))
	require.Error(t, s.PutMailboxChunk(ctx, req.Owner, req.ID, MailboxChunk{Index: 0, SHA256: fakeHash("bb"), Size: 3}))
	sealed, err := s.SealMailboxContainer(ctx, req.Owner, req.ID, req.SHA256, 3)
	require.NoError(t, err)
	require.Equal(t, "sealed", sealed.State)
	require.Len(t, sealed.Chunks, 1)
	require.Error(t, s.AbortMailboxContainer(ctx, req.Owner, req.ID))
	_, err = s.db.Exec(`UPDATE mailbox_containers SET created_at=?`, time.Now().Add(-48*time.Hour).UTC().Format(timestampLayout))
	require.NoError(t, err)
	require.NoError(t, s.CleanupMailboxContainers(ctx))
	_, err = s.MailboxContainer(ctx, req.Owner, req.ID)
	require.NoError(t, err)
	var backup bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.NoError(t, snapshot.Close())
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	got, err := restored.MailboxContainer(ctx, req.Owner, req.ID)
	require.NoError(t, err)
	require.Equal(t, sealed.Chunks, got.Chunks)
	require.NoError(t, restored.ValidateMetadata(ctx))
}

func TestMailboxContainerMaximumOrderedManifest(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	c := MailboxContainerRequest{ID: "maximum", Owner: "one", SHA256: fakeHash("aa"), Size: 256 << 30, Format: "zip"}
	_, err := s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	hash := fakeHash("bb")
	require.NoError(t, s.RecordRenditionBlob(ctx, hash, 64<<20, BlobPhysical{Encoding: "raw", StoredBytes: 64 << 20, PackEligible: true, Created: true}))
	// Seed a catalog-only repeated-chunk boundary fixture, not physical bytes.
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, `INSERT INTO mailbox_chunks(container_id,chunk_index,blob_hash,size) VALUES(?,?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { _ = statement.Close() }()
		for index := range 4095 {
			if _, err = statement.ExecContext(ctx, c.ID, index, hash, 64<<20); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 4095, SHA256: hash, Size: 64 << 20}))
	require.Error(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 4096, SHA256: hash, Size: 64 << 20}))
	sealed, err := s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	require.Len(t, sealed.Chunks, 4096)
	require.Equal(t, int64(256<<30), sealed.Size)
	var out bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &out))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(out.Bytes())))
	got, err := restored.MailboxContainer(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	require.Equal(t, sealed.ManifestSHA256, got.ManifestSHA256)
}

func TestMailboxContainerQuotaExpiryAndCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := MailboxContainerRequest{ID: "a", Owner: "one", SHA256: fakeHash("aa"), Size: 256 << 30, Format: "zip"}
	_, err := s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	req.ID = "b"
	_, err = s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	req.ID = "c"
	_, err = s.BeginMailboxContainer(ctx, req)
	require.Error(t, err)
	_, err = s.db.Exec(`UPDATE mailbox_containers SET created_at=?`, time.Now().Add(-25*time.Hour).UTC().Format(timestampLayout))
	require.NoError(t, err)
	_, err = s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	req.ID = "d"
	req.Size++
	_, err = s.BeginMailboxContainer(ctx, req)
	require.Error(t, err)
	require.NoError(t, s.AbortMailboxContainer(ctx, "one", "c"))
}
