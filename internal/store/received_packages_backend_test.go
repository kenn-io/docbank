package store

import (
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkReceivedPackageStagingAndRetention(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	for _, outcome := range []string{"cancel", "fail", "revoke-owner"} {
		t.Run(outcome, func(t *testing.T) {
			s := newStore(t)
			job, record, labels, receipt := receivedRecordPublication(t, s)
			run, err := s.PackageIngestRun(t.Context(), job.PackageID)
			require.NoError(t, err)
			renewed, err := s.RenewPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, time.Hour)
			require.NoError(t, err)
			require.Equal(t, job.Token, renewed.Token)
			require.Equal(t, job.Epoch, renewed.Epoch)
			require.Greater(t, renewed.LeaseExpiresAt, job.LeaseExpiresAt)
			_, err = s.RenewPackageImportJob(t.Context(), job.ID, job.Epoch, "stale", time.Minute)
			require.ErrorIs(t, err, ErrPackageConflict)
			var staged []ContentWriteReceipt
			for _, name := range []string{"supplied.txt", "uncommitted.txt"} {
				got, err := s.IngestPackageFileWithReceipt(t.Context(), job, run, s.RootID(), name,
					fakeHash(name), 1, "text/plain", name, BlobPhysical{Encoding: "raw", StoredBytes: 1, Created: true})
				require.NoError(t, err)
				staged = append(staged, got)
			}
			// Supplied representations belong to the receipt even though they are
			// separate nodes and are not its primary content version.
			receipt.ReceiptJSON, err = canonical.Marshal(map[string]any{"member": map[string]any{
				"representations": []map[string]any{{"content_version_id": staged[0].Node.CurrentVersionID}},
			}})
			require.NoError(t, err)
			_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
			require.NoError(t, err)
			switch outcome {
			case "cancel":
				_, err = s.CancelPackageImportJob(t.Context(), job.Owner, job.OperationID)
			case "fail":
				_, err = s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, "failed", "", nil)
			case "revoke-owner":
				err = s.RevokePackageImportOwner(t.Context(), job.Owner)
			}
			require.NoError(t, err)
			_, err = s.NodeByID(t.Context(), staged[0].Node.ID)
			require.NoError(t, err)
			_, err = s.ContentVersionByID(t.Context(), receipt.ContentVersionID)
			require.NoError(t, err)
			_, err = s.NodeByID(t.Context(), staged[1].Node.ID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.IngestPackageFileWithReceipt(t.Context(), job, run, s.RootID(), "late.txt",
				fakeHash("late"), 1, "text/plain", "late.txt", BlobPhysical{Encoding: "raw", StoredBytes: 1})
			require.ErrorIs(t, err, ErrPackageConflict)
			_, err = s.NodeByPath(t.Context(), "/late.txt")
			require.ErrorIs(t, err, ErrNotFound)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkReceivedPackageAdmissionRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	pkg := validPackageRequest(t, s)
	run, err := s.BeginIngest(t.Context(), "package:loadfile", "synthetic-admission")
	require.NoError(t, err)
	pkg.Direction, pkg.State, pkg.SnapshotID, pkg.IngestID = "received", "importing", "", run.ID()
	pkg.ManifestSHA256 = pkg.ManifestBlobSHA256
	request := packageImportJobRequest(t, s, pkg)
	var role string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(t.Context(), "REVOKE INSERT ON documents.package_import_jobs FROM "+role)
	require.NoError(t, err)
	_, err = s.AdmitPackageImport(t.Context(), run, pkg, request)
	require.ErrorContains(t, err, "permission denied")
	_, err = s.Package(t.Context(), pkg.PackageID)
	require.ErrorIs(t, err, ErrNotFound)
	var retained int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM ingests WHERE id=$1`, run.ID()).Scan(&retained))
	require.Zero(t, retained)
	_, err = setup.ExecContext(t.Context(), "GRANT INSERT ON documents.package_import_jobs TO "+role)
	require.NoError(t, err)
	first, err := s.AdmitPackageImport(t.Context(), run, pkg, request)
	require.NoError(t, err)
	replay, err := s.AdmitPackageImport(t.Context(), run, pkg, request)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	checkRetentionMetadata(t, s)
}

func checkReceivedPackagePublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"record", "complete", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			job, record, labels, receipt := receivedRecordPublication(t, s)
			var role string
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
			role = pgx.Identifier{role}.Sanitize()
			if operation == "record" {
				_, err := setup.ExecContext(t.Context(), "REVOKE INSERT ON documents.package_import_receipts FROM "+role)
				require.NoError(t, err)
				_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
				require.ErrorContains(t, err, "permission denied")
				_, err = s.PackageRecord(t.Context(), job.PackageID, record.RowID)
				require.ErrorIs(t, err, ErrNotFound)
				rows, err := s.PackageLabels(t.Context(), job.PackageID, record.OccurrenceID)
				require.NoError(t, err)
				require.Empty(t, rows)
				_, err = setup.ExecContext(t.Context(), "GRANT INSERT ON documents.package_import_receipts TO "+role)
				require.NoError(t, err)
				_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
				require.NoError(t, err)
				checkRetentionMetadata(t, s)
				return
			}
			version, err := s.ContentVersionByID(t.Context(), receipt.ContentVersionID)
			require.NoError(t, err)
			if operation == "complete" {
				_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
				require.NoError(t, err)
			}
			before, err := s.Package(t.Context(), job.PackageID)
			require.NoError(t, err)
			snapshotID := uuid.New().String()
			member := CollectionSnapshotMember{Ordinal: 1, OccurrenceID: record.OccurrenceID, NodeID: version.NodeID,
				ContentVersionID: version.ID, BlobSHA256: version.BlobHash, Size: version.Size,
				FamilyID: record.OccurrenceID, FamilyOrder: 1, DocumentKind: "other", FrozenFieldsJSON: "{}"}
			finish := func() error {
				if operation == "cancel" {
					_, err := s.CancelPackageImportJob(t.Context(), job.Owner, job.OperationID)
					return err
				}
				_, err := s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token,
					"partial", snapshotID, streamSnapshotMembers(t, member))
				return err
			}
			// The last update fails after the snapshot/package or cleanup writes.
			_, err = setup.ExecContext(t.Context(), "REVOKE UPDATE(state) ON documents.package_import_jobs FROM "+role)
			require.NoError(t, err)
			require.ErrorContains(t, finish(), "permission denied")
			after, err := s.Package(t.Context(), job.PackageID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			current, err := s.PackageImportJob(t.Context(), job.Owner, job.OperationID)
			require.NoError(t, err)
			require.Equal(t, job, current)
			_, err = s.NodeByID(t.Context(), version.NodeID)
			require.NoError(t, err)
			_, err = s.CollectionSnapshot(t.Context(), snapshotID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = setup.ExecContext(t.Context(), "GRANT UPDATE(state) ON documents.package_import_jobs TO "+role)
			require.NoError(t, err)
			require.NoError(t, finish())
			after, err = s.Package(t.Context(), job.PackageID)
			require.NoError(t, err)
			if operation == "complete" {
				require.Equal(t, "partial", after.State)
				require.Equal(t, snapshotID, after.SnapshotID)
			} else {
				require.Equal(t, "cancelled", after.State)
				_, err = s.NodeByID(t.Context(), version.NodeID)
				require.ErrorIs(t, err, ErrNotFound)
			}
			checkRetentionMetadata(t, s)
		})
	}
}

func checkReceivedPackageReadSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"progress", "timeline"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			job, record, labels, receipt := receivedRecordPublication(t, s)
			_, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
			require.NoError(t, err)
			key, err := PackageRecordKey("VOL001.dat", 2, "DOC-B")
			require.NoError(t, err)
			record.RowID, record.RowOrdinal, record.OccurrenceID = key, 2, PackageOccurrenceID(job.PackageID, key)
			receipt.ReceiptID, receipt.RecordKey, receipt.OccurrenceID = uuid.New().String(), key, record.OccurrenceID
			if operation == "progress" {
				receipt.State, receipt.ContentVersionID, receipt.ReceiptJSON = "rejected", "", []byte(`{"gaps":[]}`)
			}
			var writeErr error
			reader := sealedDeliveryReaderAfterSelect(t, s, func() {
				if operation == "progress" {
					_, writeErr = s.RecordPackageGapWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, receipt)
				} else {
					_, writeErr = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, nil, receipt)
				}
			})
			if operation == "progress" {
				before, err := reader.PackageImportProgress(t.Context(), job.PackageID)
				require.NoError(t, writeErr)
				require.NoError(t, err)
				require.Equal(t, PackageImportProgress{Committed: 1, Gaps: []string{}}, before)
				after, err := s.PackageImportProgress(t.Context(), job.PackageID)
				require.NoError(t, err)
				require.Equal(t, PackageImportProgress{Committed: 1, GapCount: 1, Gaps: []string{key}}, after)
			} else {
				before, err := reader.PackageTimelineInputs(t.Context(), job.PackageID, "", 10)
				require.NoError(t, writeErr)
				require.NoError(t, err)
				require.Len(t, before, 1)
				after, err := s.PackageTimelineInputs(t.Context(), job.PackageID, "", 10)
				require.NoError(t, err)
				require.Len(t, after, 2)
			}
		})
	}
}
