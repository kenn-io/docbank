package store

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkRenditionWorkerResume(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	versions := seedRenditionCatalogVersions(t, s)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	claim, found, err := s.ClaimNextRenditionJob(ctx, "first-worker", at, time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, job.ID, claim.JobID)
	work, err := s.RenditionJobWorkByClaim(ctx, claim, at)
	require.NoError(t, err)
	require.Equal(t, waiter.ID, work.Waiter.ID)
	snapshot := renditionJobTestSnapshot(request)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID, at, snapshot)
	require.NoError(t, err)
	require.NoError(t, s.CheckpointRenditionProvider(ctx, claim, "synthetic-resume-handle", at))
	renewed, err := s.RenewRenditionJobClaim(ctx, claim, at.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	require.Equal(t, at.Add(90*time.Second), renewed.LeaseExpires)
	_, found, err = s.ClaimNextRenditionJob(ctx, "second-worker", at.Add(70*time.Second), time.Minute)
	require.NoError(t, err)
	require.False(t, found, "renewal keeps another worker from reclaiming the job")
	resumed, found, err := s.ClaimNextRenditionJob(ctx, "second-worker", at.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, resumed.Epoch, claim.Epoch)
	require.Equal(t, "synthetic-resume-handle", resumed.ResumeHandle)
	work, err = s.RenditionJobWorkByClaim(ctx, resumed, at.Add(2*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, work.ExecutionSnapshot)
	require.Equal(t, snapshot, *work.ExecutionSnapshot)
	require.ErrorIs(t, s.CheckpointRenditionProvider(ctx, claim, "stale-handle", at.Add(2*time.Minute)), ErrRenditionJobFenced)
	require.NoError(t, s.MarkRenditionJobRetry(ctx, resumed, RenditionFailureTransient, at.Add(2*time.Minute), at.Add(4*time.Minute)))
	_, found, err = s.ClaimNextRenditionJob(ctx, "third-worker", at.Add(3*time.Minute), time.Minute)
	require.NoError(t, err)
	require.False(t, found, "retry waits until its scheduled time")
	resumed, found, err = s.ClaimNextRenditionJob(ctx, "third-worker", at.Add(4*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "synthetic-resume-handle", resumed.ResumeHandle)
	require.Equal(t, 1, resumed.ProviderAttempts, "resuming must not consume a fresh provider attempt")
	checkRetentionMetadata(t, s)
}

func checkRenditionSnapshotsPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"build", "active", "metadata", "current attachment", "text"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			versions := seedRenditionCatalogVersions(t, s)
			ctx := t.Context()
			profile := catalogProcessingProfile(t, false)
			build := catalogRenditionBuild(s, profile)
			require.NoError(t, s.StageRenditionBuild(ctx, build))
			attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}
			generationID := testSHA256([]byte("snapshot-generation"))
			require.NoError(t, publishRenditionForTest(t, s, attachment, nowRFC3339(), generationID))
			node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
			require.NoError(t, err)
			read := func(s *Store) (any, error) {
				switch operation {
				case "build":
					return s.RenditionBuild(ctx, build.ID)
				case "active":
					return s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
				case "metadata":
					return s.ActiveRenditionMetadata(ctx, versions[0], profile.Fingerprint)
				case "current attachment":
					_, view, err := s.CurrentRenditionByAttachment(ctx, node.ID, versions[0], node.BlobHash, attachment.ID)
					return view, err
				default:
					return s.ResolveRenditionText(ctx, RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision, ContentVersionID: versions[0], SourceSHA256: node.BlobHash, SourceSize: node.Size, ProfileFingerprint: profile.Fingerprint, GenerationID: generationID})
				}
			}
			before, err := read(s)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var cfg *pgx.ConnConfig
			err = conn.Raw(func(c any) error {
				pgConn, ok := c.(*stdlib.Conn)
				if !ok {
					return fmt.Errorf("unexpected PostgreSQL connection %T", c)
				}
				cfg = pgConn.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			var purged int
			var purgeErr error
			cfg.Tracer = &afterFirstSelect{run: func() {
				report, err := s.PurgeDerivatives(ctx, PurgeRequest{All: true})
				purged, purgeErr = report.RemovedBuilds, err
			}}
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
			during, err := read(reader)
			require.NoError(t, purgeErr)
			require.Equal(t, 1, purged)
			require.NoError(t, err)
			require.Equal(t, before, during)
			_, err = read(s)
			require.ErrorIs(t, err, ErrNotFound, "the next read sees the committed purge")
		})
	}
}

func checkRenditionPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	versions := seedRenditionCatalogVersions(t, s)
	profile := catalogProcessingProfile(t, false)
	original := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, original))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: original.ID, Profile: profile, AttachedAt: nowRFC3339()}
	generationID := testSHA256([]byte("rollback-original-generation"))
	require.NoError(t, publishRenditionForTest(t, s, attachment, nowRFC3339(), generationID))
	before, err := s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
	require.NoError(t, err)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "publication-worker", at, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID, at, renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	// Fail after the build and artifacts have been inserted, before units exist.
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.rendition_units FROM "+role)
	require.NoError(t, err)
	require.ErrorContains(t, s.StageRenditionJobBuild(ctx, claim, build, at), "permission denied")
	_, err = s.RenditionBuild(ctx, build.ID)
	require.ErrorIs(t, err, ErrNotFound)
	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, RenditionPhaseProvider, current.Phase)
	var artifacts int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM rendition_artifacts WHERE build_id=$1`, build.ID).Scan(&artifacts))
	require.Zero(t, artifacts)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.rendition_units TO "+role)
	require.NoError(t, err)
	require.NoError(t, s.StageRenditionJobBuild(ctx, claim, build, at))
	replacement := testSHA256([]byte("rollback-replacement-generation"))
	_, err = s.StageRenditionJobGeneration(ctx, claim, replacement, at)
	require.NoError(t, err)
	// Fail after both serving heads have changed, inside the same transaction.
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.rendition_lexical_superseded FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(ctx, claim, at)
	require.ErrorContains(t, err, "permission denied")
	after, err := s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, before, after)
	var lexicalHead string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`).Scan(&lexicalHead))
	require.Equal(t, generationID, lexicalHead)
	waiting, err := s.RenditionJobWaiterByID(ctx, waiter.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", waiting.State)
	current, err = s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, RenditionPhaseGenerationStaged, current.Phase)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.rendition_lexical_superseded TO "+role)
	require.NoError(t, err)
	published, err := s.PublishRenditionJob(ctx, claim, at)
	require.NoError(t, err)
	require.Equal(t, 1, published.PublishedWaiterCount)
	after, err = s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, build.ID, after.Build.ID)
	checkRetentionMetadata(t, s)
}

func checkRenditionRetryErrorsPostgres(t *testing.T) {
	t.Helper()
	s := &Store{postgresSchema: "documents"}
	for _, tc := range []struct {
		code  string
		retry bool
	}{
		{"40001", true}, {"40P01", true}, {"55P03", true}, {"23505", false}, {"42501", false}, {"57014", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			require.Equal(t, tc.retry, s.RenditionJobErrorRetryable(fmt.Errorf("catalog operation: %w", &pgconn.PgError{Code: tc.code})))
			require.Equal(t, tc.retry, s.RenditionJobErrorRetryable(fmt.Errorf("%w: %w", errCommitTx, &pgconn.PgError{Code: tc.code})))
		})
	}
	require.False(t, s.RenditionJobErrorRetryable(ErrRenditionJobFenced))
	require.False(t, s.RenditionJobErrorRetryable(nil))
}

func checkRenditionArtifactStaging(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	_, err := s.CreateFile(ctx, s.RootID(), "source.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	build.PartialSuccess, build.Truncated = true, true
	for _, artifact := range build.Artifacts {
		require.NoError(t, s.RecordRenditionBlob(ctx, artifact.BlobHash, artifact.Size, BlobPhysical{Encoding: "raw", StoredBytes: artifact.Size, PackEligible: true, Created: true}))
	}
	var staged int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM rendition_blob_staging`).Scan(&staged))
	require.Equal(t, 2, staged)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM rendition_blob_staging`).Scan(&staged))
	require.Zero(t, staged, "a committed build owns the artifacts instead of staging")
	got, err := s.RenditionBuild(ctx, build.ID)
	require.NoError(t, err)
	require.Equal(t, build, got)
	checkRetentionMetadata(t, s)
}

func checkRenditionReplacementEmbeddingsPostgres(t *testing.T) {
	t.Helper()
	source, version, profile, attachment := newEmbeddingCatalogFixture(t)
	ctx := t.Context()
	chunk := embeddingSetFixture(ctx, source, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	direct := embeddingSetFixture(ctx, source, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	for _, record := range []EmbeddingSetRecord{chunk, direct} {
		require.NoError(t, source.StageEmbeddingSet(ctx, record))
		require.NoError(t, source.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{FencingToken: 1,
			Key:   EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
		binding := ""
		if record.InputKind == document.EmbeddingInputRenditionChunk {
			binding = attachment
		}
		require.NoError(t, source.RecordEmbeddingFailure(ctx, EmbeddingFailureRecord{ContentVersionID: version,
			ProcessingProfileFingerprint: profile.Fingerprint, BindingID: record.BindingID, InputKind: record.InputKind,
			AttachmentID: binding, FencingToken: 2, FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime}))
	}
	// Embedding production remains unqualified. Only the replacement and its
	// effects run natively on PostgreSQL, starting from valid retained evidence.
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		build := catalogRenditionBuild(s, profile)
		build.ID = testSHA256([]byte("replacement-embedding-build"))
		build.CapturedArtifactPolicy = jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
		build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
		require.NoError(t, s.StageRenditionBuild(ctx, build))
		replacement := RenditionAttachmentRecord{ID: testSHA256([]byte("replacement-embedding-attachment")), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
		require.NoError(t, publishRenditionForTest(t, s, replacement, embeddingCatalogTime, testSHA256([]byte("replacement-embedding-generation"))))
		for _, record := range []EmbeddingSetRecord{chunk, direct} {
			var heads, failures int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM embedding_heads WHERE embedding_set_id=$1`, record.ID).Scan(&heads))
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM embedding_failures WHERE binding_id=$1`, record.BindingID).Scan(&failures))
			if record.InputKind == document.EmbeddingInputRenditionChunk {
				require.Zero(t, heads)
				require.Zero(t, failures)
			} else {
				require.Equal(t, 1, heads)
				require.Equal(t, 1, failures)
			}
		}
		checkRetentionMetadata(t, s)
	}
}
