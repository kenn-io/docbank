package docbank

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"path/filepath"
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

func TestVaultMailboxTransferNewestReceiptSurvivesRestore(t *testing.T) {
	ctx := t.Context()
	v, err := New(ctx, Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, v.Close()) })
	const archive, reference = "synthetic-restore", "message-one"
	require.NoError(t, v.RegisterMailboxArchive(ctx, archive, "Synthetic restore order"))
	request := MailboxTransferRequest{ArchiveID: archive, Reference: reference, Settings: "mime-v1", DestinationID: v.metadata.RootID(), Name: "message.eml"}
	var newest MailboxTransferReceipt
	var largestID string
	for attempt := range 64 {
		raw := []byte(fmt.Sprintf("Subject: Revision %d\nContent-Type: text/plain\n\nSynthetic body %d.\n", attempt, attempt))
		digest := sha256.Sum256(raw)
		request.SHA256, request.Size = hex.EncodeToString(digest[:]), int64(len(raw))
		if attempt > 0 {
			request.ExpectedRevision = new(newest.TargetRevision)
		}
		newest, err = v.TransferEML(ctx, request, bytes.NewReader(raw))
		require.NoError(t, err)
		if newest.ID < largestID {
			break
		}
		largestID = newest.ID
	}
	require.Less(t, newest.ID, largestID, "the newest receipt must precede an older receipt in UUID order")
	var exported bytes.Buffer
	require.NoError(t, v.metadata.ExportMetadata(ctx, &exported))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, &exported))
	for _, source := range []*store.Store{v.metadata, restored} {
		selected, err := source.MailboxTransfer(ctx, "vault:"+v.metadata.VaultID(), archive, reference)
		require.NoError(t, err)
		require.Equal(t, newest, selected)
	}
}
