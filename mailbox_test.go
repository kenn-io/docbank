package docbank

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestVaultMailboxTransferRevisionAndTombstone(t *testing.T) {
	ctx := t.Context()
	v, err := New(ctx, Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, v.Close()) })
	require.NoError(t, v.RegisterMailboxArchive(ctx, "synthetic-export", "Explicit exported email"))
	raw := []byte("Subject: First\nContent-Type: text/plain\n\nSynthetic body.\n")
	h := sha256.Sum256(raw)
	request := MailboxTransferRequest{ArchiveID: "synthetic-export", Reference: "message-one", SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw)), Settings: "mime-v1", DestinationID: v.metadata.RootID(), Name: "message.eml"}
	first, err := v.TransferEML(ctx, request, bytes.NewReader(raw))
	require.NoError(t, err)
	retry, err := v.TransferEML(ctx, request, bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, first, retry)
	changed := []byte("Subject: Second\nContent-Type: text/plain\n\nChanged synthetic body.\n")
	h = sha256.Sum256(changed)
	request.SHA256 = hex.EncodeToString(h[:])
	request.Size = int64(len(changed))
	_, err = v.TransferEML(ctx, request, bytes.NewReader(changed))
	require.ErrorIs(t, err, ErrMailboxConflict)
	request.ExpectedRevision = &first.TargetRevision
	request.Name = "remapped.eml"
	_, err = v.TransferEML(ctx, request, bytes.NewReader(changed))
	require.ErrorIs(t, err, ErrMailboxConflict)
	request.Name = "message.eml"
	second, err := v.TransferEML(ctx, request, bytes.NewReader(changed))
	require.NoError(t, err)
	require.Equal(t, first.Target.NodeID, second.Target.NodeID)
	require.NotEqual(t, first.Target.VersionID, second.Target.VersionID)
	_, _, err = v.metadata.Trash(ctx, second.Target.NodeID, second.TargetRevision)
	require.NoError(t, err)
	tombstone, err := v.TransferEML(ctx, request, bytes.NewReader(changed))
	require.NoError(t, err)
	require.Equal(t, "tombstone", tombstone.Outcome)
	require.Equal(t, second.ID, tombstone.ID)
	require.NoError(t, v.metadata.ValidateMetadata(ctx))
}
