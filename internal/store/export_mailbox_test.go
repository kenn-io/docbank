package store

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func TestExportCompletedMailboxCollectionPinsImportedVersions(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "seed.eml")
	source, err := emailVersion(t.Context(), s.db, f.publication.ContentVersionID)
	require.NoError(t, err)
	c := MailboxContainerRequest{ID: "source", Owner: "one", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(t.Context(), c)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(t.Context(), c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(t.Context(), c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(t.Context(), c.Owner, MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	j, err := s.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(t.Context(), MailboxArchive{ID: "mailbox:" + j.CollectionID, Owner: c.Owner, Description: "Synthetic"}))
	settings, err := j.Settings.Canonical()
	require.NoError(t, err)
	location := MailboxLocation{ContainerID: c.ID, Entry: "source.mbox", EntrySHA256: c.SHA256, Sequence: 1, End: c.Size, RawSHA256: c.SHA256, EMLSHA256: c.SHA256, EMLSize: c.Size, Separator: "From synthetic", Labels: []string{}}
	o := MailboxOccurrence{JobID: j.ID, Ordinal: 1, Location: location}
	p := MailboxTransferPublication{Owner: c.Owner, Request: MailboxTransferRequest{ArchiveID: "mailbox:" + j.CollectionID, Reference: "0:1", SHA256: c.SHA256, Size: c.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: f.publication, Location: &location}
	require.NoError(t, s.CommitMailboxOccurrence(t.Context(), j.ID, j.Claim, o, &p))
	var r bundle.SourceRequest
	request := func() bundle.SourceRequest {
		raw := fmt.Sprintf(`{"operation_id":%q,"kind":"mailbox_collection","collection_id":%q}`, uuid.NewString(), j.CollectionID)
		require.NoError(t, json.Unmarshal([]byte(raw), &r))
		return r
	}
	_, err = s.CreateExportSource(t.Context(), c.Owner, request(), nil)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.NoError(t, s.FinishMailboxJob(t.Context(), j.ID, j.Claim, "complete", "", true))
	r = request()
	frozen, err := s.CreateExportSource(t.Context(), c.Owner, r, nil)
	require.NoError(t, err)
	require.Equal(t, 1, frozen.Total)
	items, err := s.MailboxOccurrences(t.Context(), c.Owner, j.ID, 0, 100)
	require.NoError(t, err)
	member := items[0].Target
	_, _, err = s.ReplaceContent(t.Context(), member.NodeID, UnconditionalRev, fakeHash("ff"), 19, "message/rfc822")
	require.NoError(t, err)
	retried, err := s.CreateExportSource(t.Context(), c.Owner, r, nil)
	require.NoError(t, err)
	require.Equal(t, frozen, retried)
	plan, err := s.CreateExportPlan(t.Context(), c.Owner, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: frozen.ID, MemberHash: frozen.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	require.NoError(t, s.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		require.Equal(t, member.VersionID, d.VersionID)
		require.Equal(t, member.SHA256, d.SHA256)
		return nil
	}))
	_, err = s.CreateExportSource(t.Context(), "other", request(), nil)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestExportPublicationReleasedOnlyAfterRetentionCleanup(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	p, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "retained-publication"))
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{view.Version.NodeID}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "attachment_original"}}})
	require.NoError(t, err)
	require.ErrorIs(t, s.RemoveEmailDocumentPublication(t.Context(), p.OperationID, p.RequestDigest), bundle.ErrRetained)
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_plans SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, plan.ID)
	require.NoError(t, err)
	require.NoError(t, s.CleanupExportAuthority(t.Context()))
	require.NoError(t, s.RemoveEmailDocumentPublication(t.Context(), p.OperationID, p.RequestDigest))
}
