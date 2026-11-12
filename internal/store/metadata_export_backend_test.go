package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkMetadataExportRecovery(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	s := open(t)
	run, err := s.BeginIngest(ctx, "cli", "metadata recovery")
	require.NoError(t, err)
	file, err := s.IngestFileExact(ctx, run, s.RootID(), "report.txt", fakeHash("a1"), 9, "text/plain", "report.txt", "")
	require.NoError(t, err)
	file, _, err = s.ReplaceContent(ctx, file.ID, file.Revision, fakeHash("a2"), 11, "text/plain")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "retained")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, file.ID, file.Revision)
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Example Person", "operator")
	require.NoError(t, err)
	active, err := s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Active Party", PersonID: &person.PersonID})
	require.NoError(t, err)
	retired, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Retired Party", PersonID: &person.PersonID})
	require.NoError(t, err)
	retired, err = s.RetireParty(ctx, retired.PartyID, retired.Revision)
	require.NoError(t, err)
	party, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Contract owner", Own: true})
	require.NoError(t, err)
	contract, err := s.CreateContract(ctx, ContractInput{Title: "Retained agreement", Parties: []ContractPartyInput{{PartyID: party.PartyID, Role: "own"}}})
	require.NoError(t, err)
	deleted, err := s.Mkdir(ctx, s.RootID(), "removed")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, deleted.ID, deleted.Revision)
	require.NoError(t, err)
	purged, err := s.TrashEmptyBounded(ctx, 0, 10, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), purged.Deleted)
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	first, err := s.VerifyAudit(ctx, nil)
	require.NoError(t, err)
	file, err = s.NodeByID(ctx, file.ID)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, file.ID, s.RootID(), "renamed-report.txt", file.Revision)
	require.NoError(t, err)
	current, err := s.VerifyAudit(ctx, &first.Evidence)
	require.NoError(t, err)
	require.True(t, current.EvidenceCheck.Extends)
	require.Equal(t, int64(20), current.ProtectedBytes)

	var exported, repeated bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NoError(t, s.ExportMetadata(ctx, &repeated))
	require.Equal(t, exported.Bytes(), repeated.Bytes())
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	require.Equal(t, s.VaultID(), target.VaultID())
	require.NoError(t, target.ValidateMetadata(ctx))
	for _, want := range []Party{active, retired} {
		got, err := target.GetParty(ctx, want.PartyID)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	recoveredContract, err := target.GetContract(ctx, contract.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, recoveredContract.Parties, 1)
	require.Equal(t, party.PartyID, recoveredContract.Parties[0].PartyID)
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	require.Equal(t, exported.Bytes(), restored.Bytes())
	recovered, err := target.VerifyAudit(ctx, &first.Evidence)
	require.NoError(t, err)
	require.Equal(t, current, recovered)
	last, err := target.Mkdir(ctx, target.RootID(), "after-recovery")
	require.NoError(t, err)
	require.Equal(t, deleted.ID+1, last.ID, "recovery preserves allocation authority above the highest surviving node")
	_, err = target.VerifyAudit(ctx, &current.Evidence)
	require.NoError(t, err)
}

func checkMetadataSnapshotPinnedPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	_, err := s.CreateFile(ctx, s.RootID(), "before.txt", fakeHash("c1"), 7, "text/plain")
	require.NoError(t, err)
	var before bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	_, err = s.CreateFile(ctx, s.RootID(), "after.txt", fakeHash("c2"), 8, "text/plain")
	require.NoError(t, err)
	var pinned, backup, after bytes.Buffer
	require.NoError(t, snapshot.Export(ctx, &pinned))
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.Equal(t, before.Bytes(), pinned.Bytes())
	require.Equal(t, pinned.Bytes(), backup.Bytes())
	var retained int
	require.NoError(t, snapshot.QueryRowContext(ctx, BackupBlobAuthorityCTE()+`SELECT COUNT(*) FROM backup_authorized_blobs`).Scan(&retained))
	require.Equal(t, 1, retained)
	require.NoError(t, s.ExportMetadata(ctx, &after))
	require.NotEqual(t, pinned.Bytes(), after.Bytes())
}

func checkMetadataRetainedCatalogsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for name, seed := range retainedMetadataCatalogSeeds() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := seed(t)
			postgres, _ := copyRetentionStore(t, source)
			var want, got bytes.Buffer
			require.NoError(t, source.ExportMetadata(t.Context(), &want))
			require.NoError(t, postgres.ExportMetadata(t.Context(), &got))
			require.Equal(t, want.Bytes(), got.Bytes())
			// Reuse the released logical reader, not a test SQL copier, for recovery.
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(got.Bytes())))
			var after bytes.Buffer
			require.NoError(t, restored.ExportMetadata(t.Context(), &after))
			require.Equal(t, want.Bytes(), after.Bytes())
		})
	}
}

func checkMetadataBackupAuthority(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "retained.txt", fakeHash("b1"), 5, "text/plain")
	require.NoError(t, err)
	staged := fakeHash("b2")
	require.NoError(t, s.RecordRenditionBlob(ctx, staged, 7,
		BlobPhysical{Encoding: "raw", StoredBytes: 7, PackEligible: true, Created: true}))
	// The same staging mechanism must not hide bytes retained by a document.
	require.NoError(t, s.RecordRenditionBlob(ctx, file.BlobHash, 5,
		BlobPhysical{Encoding: "raw", StoredBytes: 5, PackEligible: true, Created: true}))
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	var exported, backup bytes.Buffer
	require.NoError(t, snapshot.Export(ctx, &exported))
	require.Contains(t, exported.String(), staged)
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.NotContains(t, backup.String(), staged)
	require.Contains(t, backup.String(), file.BlobHash)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, target.ValidateMetadata(ctx))
	restored, err := target.NodeByPath(ctx, "/retained.txt")
	require.NoError(t, err)
	require.Equal(t, file.CurrentVersionID, restored.CurrentVersionID)
}

func checkMetadataProcessingJobs(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	versions := seedRenditionCatalogVersions(t, s)
	original := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, s, original)
	job, waiter, err := s.EnqueueRenditionJob(ctx, original)
	require.NoError(t, err)
	asset := albumAsset(t, s, "described.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	derived := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, derived)
	omittedJob, omittedWaiter, err := s.EnqueueRenditionJob(ctx, derived)
	require.NoError(t, err)
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	for _, export := range []func(context.Context, io.Writer) error{snapshot.Export, snapshot.ExportBackup} {
		var stream bytes.Buffer
		require.NoError(t, export(ctx, &stream))
		require.Contains(t, stream.String(), `"type":"rendition_job","job_id":"`+job.ID+`"`)
		require.Contains(t, stream.String(), `"waiter_id":"`+waiter.ID+`"`)
		require.NotContains(t, stream.String(), omittedJob.ID)
		require.NotContains(t, stream.String(), omittedWaiter.ID)
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(stream.Bytes())))
		require.NoError(t, restored.ValidateMetadata(ctx))
	}
}

type failedMetadataWriter struct{ err error }

func (w failedMetadataWriter) Write([]byte) (int, error) { return 0, w.err }

func checkMetadataExportFailures(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.ExportMetadata(ctx, io.Discard), context.Canceled)
	writeErr := errors.New("metadata destination unavailable")
	require.ErrorIs(t, s.ExportMetadata(t.Context(), failedMetadataWriter{writeErr}), writeErr)
	require.Zero(t, s.db.Stats().InUse)
	// Closing a failed snapshot releases its connection without closing the
	// Store's caller-owned pool or leaving a transaction behind.
	_, err := s.Mkdir(t.Context(), s.RootID(), "after-failure")
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
