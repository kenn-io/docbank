package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func exportPlanFixture(t *testing.T, s *Store, owner string) bundle.Plan {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), owner+".txt", fakeHash("e1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), owner, bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "nodes", NodeIDs: []int64{node.ID}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), owner, bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}}})
	require.NoError(t, err)
	return plan
}

func checkExportJobPublicationAndRelease(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	plan := exportPlanFixture(t, s, "owner")
	request := bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}
	job, err := s.QueueExportJob(ctx, "owner", request)
	require.NoError(t, err)
	replay, err := s.QueueExportJob(ctx, "owner", request)
	require.NoError(t, err)
	require.Equal(t, job, replay)
	_, err = s.QueueExportJob(ctx, "other", request)
	require.ErrorIs(t, err, ErrNotFound)
	changed := request
	changed.Fingerprint = fakeHash("ee")
	_, err = s.QueueExportJob(ctx, "owner", changed)
	require.ErrorIs(t, err, bundle.ErrConflict)
	claim, err := s.ClaimExportJob(ctx)
	require.NoError(t, err)
	admitted, err := s.ExportPlanForClaim(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, plan, admitted)
	require.NoError(t, s.AdvanceExportJob(ctx, claim, plan.RoleEntries, plan.RoleBytes))
	require.ErrorIs(t, s.AdvanceExportJob(ctx, claim, 0, 0), bundle.ErrConflict)
	receipt := bundle.Receipt{Format: bundle.Format, PlanFingerprint: plan.Fingerprint, SHA256: fakeHash("e2"), Size: 100, Entries: plan.ArchiveEntries()}
	require.NoError(t, s.FinishExportJob(ctx, claim, &receipt, job.ID+".zip", ""))
	finished, err := s.ExportJob(ctx, "owner", job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", finished.State)
	require.Equal(t, &receipt, finished.Receipt)
	require.ErrorIs(t, s.CheckExportClaim(ctx, claim), bundle.ErrFenced)
	retained, err := s.ExportArchiveRetained(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, retained)
	called := false
	require.ErrorIs(t, s.ReleaseExportJob(ctx, "other", job.ID, func() error { called = true; return nil }), ErrNotFound)
	require.False(t, called)
	failure := errors.New("synthetic archive removal failure")
	require.ErrorIs(t, s.ReleaseExportJob(ctx, "owner", job.ID, func() error { return failure }), failure)
	stillRetained, err := s.ExportJob(ctx, "owner", job.ID)
	require.NoError(t, err)
	require.Equal(t, finished, stillRetained)
	require.NoError(t, s.ReleaseExportJob(ctx, "owner", job.ID, func() error { called = true; return nil }))
	require.True(t, called)
	_, err = s.ExportJob(ctx, "owner", job.ID)
	require.ErrorIs(t, err, ErrNotFound)
	checkRetentionMetadata(t, s)
}

func checkExportRestartRevocationAndCleanup(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	for _, owner := range []string{"owner", "master"} {
		t.Run(owner, func(t *testing.T) {
			s := open(t)
			ctx := t.Context()
			plan := exportPlanFixture(t, s, owner)
			job, err := s.QueueExportJob(ctx, owner, bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
			require.NoError(t, err)
			old, err := s.ClaimExportJob(ctx)
			require.NoError(t, err)
			require.NoError(t, s.RequeueExportJobs(ctx))
			require.ErrorIs(t, s.CheckExportClaim(ctx, old), bundle.ErrFenced)
			current, err := s.ClaimExportJob(ctx)
			require.NoError(t, err)
			require.Greater(t, current.Epoch, old.Epoch)
			require.NoError(t, s.RevokeAbandonedExportSessions(ctx))
			if owner == "master" {
				require.NoError(t, s.CheckExportClaim(ctx, current))
				require.NoError(t, s.RevokeExportOwner(ctx, owner))
			}
			require.ErrorIs(t, s.CheckExportClaim(ctx, current), bundle.ErrFenced)
			require.NoError(t, s.CancelExportJob(ctx, owner, job.ID))
			_, err = s.db.ExecContext(ctx, `UPDATE export_jobs SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=$1`, job.ID)
			require.NoError(t, err)
			expired, err := s.ExpiredExportJobs(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{job.ID}, expired)
			require.NoError(t, s.DeleteExpiredExportJob(ctx, job.ID))
			_, err = s.db.ExecContext(ctx, `UPDATE export_plans SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=$1`, plan.ID)
			require.NoError(t, err)
			_, err = s.db.ExecContext(ctx, `UPDATE export_sources SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=$1`, plan.Source.ID)
			require.NoError(t, err)
			require.NoError(t, s.CleanupExportAuthority(ctx))
			var roots int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM export_members)+(SELECT count(*) FROM export_documents)+(SELECT count(*) FROM export_role_roots)`).Scan(&roots))
			require.Zero(t, roots)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkExportFailedResolutionKeepsReservation(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "resolution.txt", fakeHash("e3"), 1, "text/plain")
	require.NoError(t, err)
	request := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "nodes", NodeIDs: []int64{node.ID}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	resolve := func(ctx context.Context) ([]bundle.Member, error) { calls++; cancel(); return nil, ctx.Err() }
	_, err = s.CreateExportSource(ctx, "owner", request, resolve)
	require.ErrorIs(t, err, context.Canceled)
	failed, err := s.ExportSource(t.Context(), "owner", request.OperationID)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.State)
	_, err = s.CreateExportSource(t.Context(), "owner", request, resolve)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.Equal(t, 1, calls)
}

func checkExportPlanRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "rollback.txt", fakeHash("e4"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "nodes", NodeIDs: []int64{node.ID}}, nil)
	require.NoError(t, err)
	request := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}}
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(canonical_json) ON documents.export_plans FROM "+role)
	require.NoError(t, err)
	_, err = s.CreateExportPlan(ctx, "owner", request)
	require.ErrorContains(t, err, "permission denied")
	var retained int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM export_plans)+(SELECT count(*) FROM export_documents)+(SELECT count(*) FROM export_role_roots)`).Scan(&retained))
	require.Zero(t, retained)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(canonical_json) ON documents.export_plans TO "+role)
	require.NoError(t, err)
	_, err = s.CreateExportPlan(ctx, "owner", request)
	require.NoError(t, err)
	checkRetentionMetadata(t, s)
}

func checkExportReadSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"plan", "preview", "problems"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			plan := exportPlanFixture(t, s, "owner")
			var writeErr error
			reader := sealedDeliveryReaderAfterSelect(t, s, func() {
				writeErr = s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE export_plans SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=$1`, plan.ID)
					return err
				})
				if writeErr == nil {
					writeErr = s.CleanupExportAuthority(ctx)
				}
			})
			switch operation {
			case "plan":
				got, err := reader.ExportPlan(ctx, "owner", plan.ID)
				require.NoError(t, err)
				require.Equal(t, plan, got)
			case "preview":
				got, err := reader.ExportPlanPreview(ctx, "owner", plan.ID)
				require.NoError(t, err)
				require.Equal(t, 1, got.Total)
				require.Equal(t, plan.Fingerprint, got.Fingerprint)
			case "problems":
				got, err := reader.ExportOutputProblems(ctx, "owner", plan.ID, 0)
				require.NoError(t, err)
				require.Equal(t, 1, got.Total)
				require.Equal(t, "text", got.Items[0].Role)
			}
			require.NoError(t, writeErr)
			_, err := s.ExportPlan(ctx, "owner", plan.ID)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func checkExportEmailInspection(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	seedRenditionCatalogVersions(t, s)
	attachment := stageEmailPDFFixture(t, s)
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
	version, err := s.ContentVersionByID(ctx, attachment.ContentVersionID)
	require.NoError(t, err)
	view, err := s.EmailMetadata(ctx, version.ID)
	require.NoError(t, err)
	for _, id := range []string{"inspection-a", "inspection-b"} {
		_, err := s.PublishEmailDocuments(ctx, attachmentRequest(t, s, view, id))
		require.NoError(t, err)
	}
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: version.NodeID, VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size}}}, nil)
	require.NoError(t, err)
	recipes, err := s.ExportEmailPDFRecipes(ctx, "owner", source.ID)
	require.NoError(t, err)
	require.Len(t, recipes.Recipes, 1)
	require.Equal(t, "A4", recipes.Recipes[0].Paper)
	require.Equal(t, 1, recipes.Recipes[0].Messages)
	choices, err := s.ExportAttachmentPublications(ctx, "owner", source.ID, 0)
	require.NoError(t, err)
	require.Equal(t, 2, choices.Total)
	require.Len(t, choices.Items, 2)
	require.Equal(t, "inspection-a", choices.Items[0].OperationID)
	require.Equal(t, "inspection-b", choices.Items[1].OperationID)
	_, err = s.CreateExportPlan(ctx, "owner", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "attachment_original"}}})
	require.ErrorIs(t, err, bundle.ErrConflict)
	plan, err := s.CreateExportPlan(ctx, "owner", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles:        []bundle.RolePolicy{{Role: "original"}, {Role: "email_pdf", RecipeSHA256: recipes.Recipes[0].RecipeSHA256}, {Role: "attachment_original"}},
		Publications: []bundle.PublicationSelection{{VersionID: version.ID, OperationID: "inspection-a"}}})
	require.NoError(t, err)
	preview, err := s.ExportPlanPreview(ctx, "owner", plan.ID)
	require.NoError(t, err)
	require.Equal(t, 1, preview.Total)
	require.Equal(t, 3, plan.RoleEntries)
	_, err = s.ExportAttachmentPublications(ctx, "other", source.ID, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ExportEmailPDFRecipes(ctx, "other", source.ID)
	require.ErrorIs(t, err, ErrNotFound)
	checkRetentionMetadata(t, s)
}

func checkExportFinishRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	plan := exportPlanFixture(t, s, "owner")
	job, err := s.QueueExportJob(ctx, "owner", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	claim, err := s.ClaimExportJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.AdvanceExportJob(ctx, claim, plan.RoleEntries, plan.RoleBytes))
	before, err := s.ExportJob(ctx, "owner", job.ID)
	require.NoError(t, err)
	var role, deadline string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user,expires_at FROM export_plans WHERE id=$1`, plan.ID).Scan(&role, &deadline))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(archive_name) ON documents.export_jobs FROM "+role)
	require.NoError(t, err)
	receipt := bundle.Receipt{Format: bundle.Format, PlanFingerprint: plan.Fingerprint, SHA256: fakeHash("e5"), Size: 100, Entries: plan.ArchiveEntries()}
	require.ErrorContains(t, s.FinishExportJob(ctx, claim, &receipt, job.ID+".zip", ""), "permission denied")
	after, err := s.ExportJob(ctx, "owner", job.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	var retained string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT expires_at FROM export_plans WHERE id=$1`, plan.ID).Scan(&retained))
	require.Equal(t, deadline, retained)
	require.NoError(t, s.CheckExportClaim(ctx, claim))
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(archive_name) ON documents.export_jobs TO "+role)
	require.NoError(t, err)
	require.NoError(t, s.FinishExportJob(ctx, claim, &receipt, job.ID+".zip", ""))
	checkRetentionMetadata(t, s)
}

func checkExplicitPhotoExportPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "render.jpg", fakeHash("e6"), "image/jpeg")
	member := bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{member}}, nil)
	require.NoError(t, err)
	request := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true, RemoveGPS: true}}
	inputs, err := s.ExportPhotoInputs(ctx, "owner", request)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	artifact := PreparedPhotoExport{Input: inputs[0], Receipt: bundle.PhotoRenderReceipt{Version: bundle.PhotoRenderReceiptVersion, Profile: request.PhotoRender.Canonical(), Source: member, Width: 10, Height: 20}, SHA256: fakeHash("e7"), Size: 99, Physical: BlobPhysical{Encoding: "raw", StoredBytes: 99, Created: true}}
	tag, err := s.CreateTag(ctx, "new-keyword")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.SealPhotoExportPlan(ctx, "owner", request, []PreparedPhotoExport{artifact})
	require.ErrorIs(t, err, bundle.ErrConflict)
	_, err = s.BlobInfo(ctx, artifact.SHA256)
	require.ErrorIs(t, err, ErrNotFound)
	inputs, err = s.ExportPhotoInputs(ctx, "owner", request)
	require.NoError(t, err)
	require.Equal(t, []string{"new-keyword"}, inputs[0].Keywords)
	artifact.Input = inputs[0]
	plan, err := s.SealPhotoExportPlan(ctx, "owner", request, []PreparedPhotoExport{artifact})
	require.NoError(t, err)
	preview, err := s.ExportPlanPreview(ctx, "owner", plan.ID)
	require.NoError(t, err)
	require.Equal(t, int64(99), preview.Roles[0].Bytes)
	checkRetentionMetadata(t, s)
}

func checkExportWorkerFailureAndCapacity(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	plan := exportPlanFixture(t, s, "owner")
	for range 2 {
		_, err := s.QueueExportJob(ctx, "owner", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
		require.NoError(t, err)
	}
	third := bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}
	_, err := s.QueueExportJob(ctx, "owner", third)
	require.ErrorIs(t, err, bundle.ErrLimit)
	claim, err := s.ClaimExportJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.FinishExportJob(ctx, claim, nil, "", "synthetic renderer failure"))
	failed, err := s.ExportJob(ctx, "owner", claim.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.State)
	require.Equal(t, "synthetic renderer failure", failed.Failure)
	require.ErrorIs(t, s.CheckExportClaim(ctx, claim), bundle.ErrFenced)
	require.NoError(t, s.ReleaseExportJob(ctx, "owner", claim.Job.ID, func() error { return nil }))
	_, err = s.QueueExportJob(ctx, "owner", third)
	require.NoError(t, err, "removing the terminal record releases its quota")
	checkRetentionMetadata(t, s)
}

func checkExportDiscoverySnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"recipes", "publications"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			seedRenditionCatalogVersions(t, s)
			attachment := stageEmailPDFFixture(t, s)
			require.NoError(t, publishAttachmentForTest(t, s, attachment))
			version, err := s.ContentVersionByID(ctx, attachment.ContentVersionID)
			require.NoError(t, err)
			mutate := func() error {
				_, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{version.ID}})
				return err
			}
			if operation == "publications" {
				view, err := s.EmailMetadata(ctx, version.ID)
				require.NoError(t, err)
				for _, id := range []string{"snapshot-a", "snapshot-b"} {
					receipt, err := s.PublishEmailDocuments(ctx, attachmentRequest(t, s, view, id))
					require.NoError(t, err)
					if id == "snapshot-a" {
						mutate = func() error { return s.RemoveEmailDocumentPublication(ctx, receipt.OperationID, receipt.RequestDigest) }
					}
				}
			}
			source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: version.NodeID, VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size}}}, nil)
			require.NoError(t, err)
			var writeErr error
			reader := sealedDeliveryReaderAfterSelect(t, s, func() { writeErr = mutate() })
			if operation == "recipes" {
				before, err := reader.ExportEmailPDFRecipes(ctx, "owner", source.ID)
				require.NoError(t, err)
				require.Len(t, before.Recipes, 1)
				after, err := s.ExportEmailPDFRecipes(ctx, "owner", source.ID)
				require.NoError(t, err)
				require.Empty(t, after.Recipes)
			} else {
				before, err := reader.ExportAttachmentPublications(ctx, "owner", source.ID, 0)
				require.NoError(t, err)
				require.Equal(t, 2, before.Total)
				after, err := s.ExportAttachmentPublications(ctx, "owner", source.ID, 0)
				require.NoError(t, err)
				require.Zero(t, after.Total, "one remaining publication is unambiguous")
			}
			require.NoError(t, writeErr)
		})
	}
}
