package store

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkEmailPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	f := newEmailFixture(t, s, "rollback.eml")
	before := emailTableCounts(t, s)
	_, err := setup.ExecContext(ctx, "REVOKE INSERT ON documents.email_heads FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishEmailGeneration(ctx, f.publication)
	require.ErrorContains(t, err, "permission denied")
	require.Equal(t, before, emailTableCounts(t, s), "inventory and staging roll back with the head")
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.email_heads TO "+role)
	require.NoError(t, err)
	view, err := s.PublishEmailGeneration(ctx, f.publication)
	require.NoError(t, err)

	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	// Fail after the child and its audited creation, before the publication receipt.
	beforeAudit, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	var beforeNodes int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM nodes`).Scan(&beforeNodes))
	request := attachmentRequest(t, s, view, "rollback-child")
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.email_document_publications FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishEmailDocuments(ctx, request)
	require.ErrorContains(t, err, "permission denied")
	afterAudit, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, beforeAudit, afterAudit)
	var afterNodes int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM nodes`).Scan(&afterNodes))
	require.Equal(t, beforeNodes, afterNodes)
	_, err = s.EmailDocumentPublication(ctx, request.OperationID)
	require.ErrorIs(t, err, ErrNotFound)
	checkRetentionMetadata(t, s)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.email_document_publications TO "+role)
	require.NoError(t, err)
	_, err = s.PublishEmailDocuments(ctx, request)
	require.NoError(t, err)

	// Fail after serving heads change, at the final body association write.
	body := stageEmailBodyForTest(t, s, view)
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.email_body_results FROM "+role)
	require.NoError(t, err)
	require.ErrorContains(t, s.PublishEmailBody(ctx, body), "permission denied")
	current, err := s.EmailMetadata(ctx, view.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", current.BodySearch.State)
	_, err = s.ActiveRendition(ctx, view.Version.ID, body.RenditionAttachment.Profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	var heads int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM rendition_lexical_heads`).Scan(&heads))
	require.Zero(t, heads)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.email_body_results TO "+role)
	require.NoError(t, err)
	require.NoError(t, s.PublishEmailBody(ctx, body))
	checkRetentionMetadata(t, s)
}

func checkPagePublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	request := pageStoreRequest(t, s)
	job, err := s.QueuePageJob(ctx, uuid.NewString(), request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(ctx)
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(ctx, claim, frames))
	recipe := pageStoreRecipe()
	image := pageStoreImage(t, frames[0], recipe)
	before, err := s.PageInventory(ctx, request.Binding())
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	// The final job result fails after recipe, blob and image insertion.
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE (results_json) ON documents.page_render_jobs FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.ErrorContains(t, s.PublishPageImage(ctx, claim, image, recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}), "permission denied")
	after, err := s.PageInventory(ctx, request.Binding())
	require.NoError(t, err)
	require.Equal(t, before, after)
	var blobs int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, image.SHA256).Scan(&blobs))
	require.Zero(t, blobs)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE (results_json) ON documents.page_render_jobs TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	for _, frame := range frames {
		require.NoError(t, s.PublishPageImage(ctx, claim, pageStoreImage(t, frame, recipe), recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))
	}
	require.NoError(t, s.FinishPageJob(ctx, claim, PageJobCompleted, ""))
	done, err := s.PageJob(ctx, job.ID, request.Binding())
	require.NoError(t, err)
	require.Equal(t, PageJobCompleted, done.State)
	require.Len(t, done.Results, 2)
	for _, frame := range frames {
		got, err := s.PageImage(ctx, request.Binding(), image.RecipeSHA256, frame.Page)
		require.NoError(t, err)
		require.Equal(t, pageStoreImage(t, frame, recipe), got.Image)
	}
	checkRetentionMetadata(t, s)
}

// The tracer commits a native write between the read's first and later SELECTs.
func emailPageSnapshotReader(t *testing.T, s *Store, change func()) *Store {
	t.Helper()
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(c any) error {
		native, ok := c.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL connection %T", c)
		}
		cfg = native.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	cfg.Tracer = &afterFirstSelect{run: change}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
}

func checkEmailPageSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Run("email body", func(t *testing.T) {
		s := newPostgresQueryStore(t)
		f := newEmailFixture(t, s, "snapshot.eml")
		view, err := s.PublishEmailGeneration(t.Context(), f.publication)
		require.NoError(t, err)
		body := stageEmailBodyForTest(t, s, view)
		before, err := s.EmailMetadata(t.Context(), view.Version.ID)
		require.NoError(t, err)
		var writeErr error
		reader := emailPageSnapshotReader(t, s, func() { writeErr = s.PublishEmailBody(t.Context(), body) })
		during, err := reader.EmailMetadata(t.Context(), view.Version.ID)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.EmailMetadata(t.Context(), view.Version.ID)
		require.NoError(t, err)
		require.Equal(t, "available", after.BodySearch.State)
		require.NotEqual(t, before, after)
	})
	t.Run("relation page", func(t *testing.T) {
		s := newPostgresQueryStore(t)
		f := newEmailFixture(t, s, "snapshot.eml")
		view, err := s.PublishEmailGeneration(t.Context(), f.publication)
		require.NoError(t, err)
		receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "snapshot"))
		require.NoError(t, err)
		query := document.EmailDocumentRelationQuery{ParentVersionID: view.Version.ID}
		before, err := s.EmailDocumentRelations(t.Context(), query)
		require.NoError(t, err)
		require.Equal(t, int64(1), before.Total)
		var writeErr error
		reader := emailPageSnapshotReader(t, s, func() {
			writeErr = s.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, receipt.RequestDigest)
		})
		during, err := reader.EmailDocumentRelations(t.Context(), query)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.EmailDocumentRelations(t.Context(), query)
		require.NoError(t, err)
		require.Zero(t, after.Total)
		require.Empty(t, after.Items)
	})
	t.Run("page inventory", func(t *testing.T) {
		s := newPostgresQueryStore(t)
		request := pageStoreRequest(t, s)
		_, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
		require.NoError(t, err)
		claim, err := s.ClaimPageJob(t.Context())
		require.NoError(t, err)
		frames := pageStoreFrames(t, request)
		require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
		before, err := s.PageInventory(t.Context(), request.Binding())
		require.NoError(t, err)
		var writeErr error
		reader := emailPageSnapshotReader(t, s, func() {
			writeErr = s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frames[0], pageStoreRecipe()), pageStoreRecipe(), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10})
		})
		during, err := reader.PageInventory(t.Context(), request.Binding())
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.PageInventory(t.Context(), request.Binding())
		require.NoError(t, err)
		require.Len(t, after.Images, 1)
		require.NotEqual(t, before, after)
	})
}
