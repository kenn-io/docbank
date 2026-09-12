package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func TestEmailPageExportAuthoritySharesBackupSnapshot(t *testing.T) {
	s := newTestStore(t)
	email, err := s.PublishEmailGeneration(t.Context(), newEmailFixture(t, s, "synthetic.eml").publication)
	require.NoError(t, err)
	request := pageStoreRequest(t, s)
	_, err = s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{
			NodeID: email.Version.NodeID, VersionID: email.Version.ID, SHA256: email.Version.BlobHash, Size: email.Version.Size,
		}},
	}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	var snapshot bytes.Buffer
	view, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, view.ExportBackup(t.Context(), &snapshot))
	require.NoError(t, view.Close())
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
	gotEmail, err := restored.EmailMetadata(t.Context(), email.Version.ID)
	require.NoError(t, err)
	require.Equal(t, email, gotEmail)
	pages, err := restored.PageInventory(t.Context(), request.Binding())
	require.NoError(t, err)
	require.Len(t, pages.Frames, 2)
	for i, frame := range frames {
		require.Equal(t, frame, pages.Frames[i].Frame)
	}
	var documents []bundle.Document
	require.NoError(t, restored.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		documents = append(documents, d)
		return nil
	}))
	require.Len(t, documents, 1)
	require.NoError(t, restored.ValidateMetadata(t.Context()))
}

func TestJoinedSchemaRejectsMissingMailboxAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(path)
	require.NoError(t, err)
	_, err = s.db.Exec(`DROP TABLE mailbox_transfer_heads`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	reopened, err := Open(path)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.ErrorContains(t, err, "unexpected mailbox_transfer_heads layout")
}
