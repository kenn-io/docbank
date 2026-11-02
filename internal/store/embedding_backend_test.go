package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkEmbeddingPublicationPostgres(t *testing.T) {
	t.Helper()
	for _, name := range []string{"optional", "chunk"} {
		t.Run(name, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s, version, profile, attachment := seedEmbeddingCatalog(t, newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner}))
			ctx := t.Context()
			binding := workerProfileEmbeddingBinding(t, profile, name)
			if binding.InputKind == document.EmbeddingInputOriginalFile {
				attachment = ""
			}
			record := embeddingSetFixture(ctx, s, version, profile.Fingerprint, binding.InputKind, name, attachment)
			var err error
			record, err = normalizeEmbeddingSetRecord(record)
			require.NoError(t, err)
			consent := ProviderOperationAuthorizationRequest{
				Principal: "operator:publication", Scope: "embedding:" + name,
				ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
				InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
			}
			_, err = s.GrantConsent(ctx, ProcessingConsentGrantRequest{
				Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
				DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
				RetainedArtifactClasses: consent.RetainedArtifactClasses,
			})
			require.NoError(t, err)
			job, err := s.EnqueueEmbeddingJob(ctx, EmbeddingJobRequest{ContentVersionID: version, Profile: profile,
				BindingID: name, Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration, Authorization: consent})
			require.NoError(t, err)
			at := time.Now().UTC()
			claim, work, found, err := s.ClaimEmbeddingWork(ctx, job.ID, "publication-worker", at, time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
			require.NoError(t, err)
			require.True(t, found)
			prior, fence, err := s.BeginEmbeddingProviderEgress(ctx, claim, work, nil, at)
			require.NoError(t, err)
			fence.Close()
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			role = pgx.Identifier{role}.Sanitize()
			// Fail after inserting the vector set, before its row manifest is complete.
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.embedding_vector_rows FROM "+role)
			require.NoError(t, err)
			require.ErrorContains(t, s.StageEmbeddingSetWithLease(ctx, record, claim.AttemptID, claim.Epoch, at), "permission denied")
			var sets int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_vector_sets`).Scan(&sets))
			require.Zero(t, sets)
			_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.embedding_vector_rows TO "+role)
			require.NoError(t, err)
			require.NoError(t, s.StageEmbeddingSetWithLease(ctx, record, claim.AttemptID, claim.Epoch, at))
			head := EmbeddingHeadRecord{Key: EmbeddingHeadKey{version, name, binding.InputKind}, SetID: record.ID,
				VectorSpaceID: work.VectorSpaceID, ProcessingProfileFingerprint: profile.Fingerprint,
				PublishedAt: at.Format(timestampLayout), FencingToken: claim.Epoch}
			// Completion follows head publication; losing its grant must roll back both.
			_, err = setup.ExecContext(ctx, "REVOKE UPDATE(state) ON documents.embedding_jobs FROM "+role)
			require.NoError(t, err)
			receipt := EmbeddingAttemptReceipt{AttemptID: job.ID}
			require.ErrorContains(t, s.PublishEmbeddingWork(ctx, claim, work, head, prior, receipt, at), "permission denied")
			var heads int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_heads`).Scan(&heads))
			require.Zero(t, heads)
			status, err := s.EmbeddingJobByID(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, "running", status.State)
			_, err = setup.ExecContext(ctx, "GRANT UPDATE(state) ON documents.embedding_jobs TO "+role)
			require.NoError(t, err)
			require.NoError(t, s.PublishEmbeddingWork(ctx, claim, work, head, prior, receipt, at))
			status, err = s.EmbeddingJobByID(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
			require.Equal(t, record.ID, embeddingHeadSetIDForTest(t, s, version, profile.Fingerprint, name, binding.InputKind))
			statuses, err := s.EmbeddingJobsForVersionProfileConsentSet(ctx, version, profile.Fingerprint, prior.GrantID)
			require.NoError(t, err)
			require.Equal(t, []EmbeddingJobStatus{status}, statuses)
			var active bool
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT active FROM current_rendition_roots WHERE root_id=$1`, job.ID).Scan(&active))
			require.False(t, active)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkEmbeddingCoverageSnapshotPostgres(t *testing.T) {
	t.Helper()
	s, version, profile, _ := seedEmbeddingCatalog(t, newPostgresQueryStore(t))
	ctx := t.Context()
	record := embeddingSetFixture(ctx, s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(ctx, record))
	require.NoError(t, s.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{Key: EmbeddingHeadKey{version, record.BindingID, record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime, FencingToken: 1}))
	scope := ProcessingCoverageScope{ContentVersionIDs: []string{version}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}}
	before, err := s.ProcessingCoverage(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, 1, before.Embeddings[0].Complete)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	err = conn.Raw(func(raw any) error {
		pg, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
		}
		config = pg.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var purged bool
	var purgeErr error
	config.Tracer = &afterFirstSelect{run: func() {
		_, purgeErr = s.PurgeDerivatives(ctx, PurgeRequest{All: true})
		purged = true
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
	during, err := reader.ProcessingCoverage(ctx, scope)
	require.NoError(t, purgeErr)
	require.True(t, purged)
	require.NoError(t, err)
	require.Equal(t, before, during)
	after, err := s.ProcessingCoverage(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, 0, after.Embeddings[0].Complete)
	require.Equal(t, 1, after.Embeddings[0].Unavailable)
}
