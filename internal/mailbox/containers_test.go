package mailbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"io"
	"path/filepath"
	"testing"
)

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func mailboxFixture(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
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
