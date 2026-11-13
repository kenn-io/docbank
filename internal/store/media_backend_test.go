package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkMediaConsentAndContinuations(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	request := suppliedMediaPublicationFixture(t, s)
	consent := testProviderAuthorizationRequest()
	consent.Principal = request.Operation.Principal
	grantOp := testMediaOperation("grant_media_acquisition", consent.Principal)
	grant, err := s.GrantMediaAcquisitionConsent(ctx, grantOp, "synthetic-origin", grantRequestForAuthorization(consent, nil))
	require.NoError(t, err)
	replay, err := s.GrantMediaAcquisitionConsent(ctx, grantOp, "synthetic-origin", grantRequestForAuthorization(consent, nil))
	require.NoError(t, err)
	require.Equal(t, grant, replay)
	authorization, err := s.AuthorizeProviderOperation(ctx, consent)
	require.NoError(t, err)
	require.Equal(t, grant.GrantID, authorization.GrantID)
	consent.PriorAuthorization = &authorization
	request.ProcessingProfile, request.ProcessingPrincipal = "speech", consent.Principal
	request.ProcessingScope, request.ProcessingProfileFingerprint = consent.Scope, consent.ProfileFingerprint
	request.ProcessingAuthorization = consent
	retained, err := s.RetainSuppliedMedia(ctx, request)
	require.NoError(t, err)
	bound, err := s.SetMediaProcessingJob(ctx, request.Operation.ID, consent.Principal, testSHA256([]byte("synthetic-job")))
	require.NoError(t, err)
	require.Equal(t, retained.SourceVersionID, bound.SourceVersionID)

	retry := request.Operation
	retry.ID, retry.Verb = "00000000-0000-4000-8000-000000000099", "retry_media"
	retryReceipt := retained
	retryReceipt.OperationID = retry.ID
	retryReceipt, err = s.QueueMediaRetry(ctx, retry, retryReceipt)
	require.NoError(t, err)
	pending, err := s.MediaProcessingContinuations(ctx, consent.Principal, "", 1)
	require.NoError(t, err)
	require.Equal(t, []MediaPublicationReceipt{bound}, pending)
	pending, err = s.MediaProcessingContinuations(ctx, consent.Principal, bound.OperationID, 1)
	require.NoError(t, err)
	require.Equal(t, []MediaPublicationReceipt{retryReceipt}, pending)
	pending, err = s.MediaProcessingContinuations(ctx, "other", "", 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	_, err = s.FailMediaProcessing(ctx, request.Operation.ID, "other")
	require.ErrorIs(t, err, ErrNotFound)
	failed, err := s.FailMediaProcessing(ctx, request.Operation.ID, consent.Principal)
	require.NoError(t, err)
	require.Equal(t, "unavailable", failed.CoverageState)
	finished, err := s.FinishMediaProcessing(ctx, retry.ID, consent.Principal, true)
	require.NoError(t, err)
	require.Equal(t, "transcribed", finished.CoverageState)
	pending, err = s.MediaProcessingContinuations(ctx, consent.Principal, "", 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	view, err := s.MediaSourceVersion(ctx, consent.Principal, retained.SourceID, retained.SourceVersionID)
	require.NoError(t, err)
	require.Equal(t, []MediaPublicationReceipt{finished, failed}, view.ProcessingReceipts)

	revokeOp := testMediaOperation("revoke_media_acquisition", consent.Principal)
	revoke := ProcessingConsentRevocationRequest{Principal: consent.Principal, Scope: consent.Scope}
	revoked, err := s.RevokeMediaAcquisitionConsent(ctx, revokeOp, "synthetic-origin", revoke)
	require.NoError(t, err)
	revokedAgain, err := s.RevokeMediaAcquisitionConsent(ctx, revokeOp, "synthetic-origin", revoke)
	require.NoError(t, err)
	require.Equal(t, revoked, revokedAgain)
	require.Greater(t, revoked.Fence, grant.Fence)
	consent.PriorAuthorization = nil
	_, err = s.AuthorizeProviderOperation(ctx, consent)
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	_, err = s.MediaSource(ctx, consent.Principal, retained.SourceID)
	require.NoError(t, err, "revoking acquisition keeps retained content readable")
	checkRetentionMetadata(t, s)
}

func checkMediaPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"supplied", "remote", "input"} {
		t.Run(operation, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			hash := testSHA256([]byte("new media content"))
			sourceID := testSHA256([]byte("new media source"))
			physical := BlobPhysical{Encoding: "raw", StoredBytes: 17, Created: true}
			op := MediaOperation{ID: "00000000-0000-4000-8000-000000000091", Principal: "operator:remote",
				Verb: "submit_supplied_media", SourceID: sourceID, RequestSHA256: testSHA256([]byte("new request"))}
			virtualPath := "/retained/media/original.wav"
			var publish func() (MediaPublicationReceipt, error)
			if operation == "supplied" {
				request := MediaPublicationRequest{Operation: op, SourceID: sourceID, VirtualPath: virtualPath,
					ContentVersion: ContentVersion{BlobHash: hash, Size: 17, MimeType: "audio/wav"}, Physical: physical,
					CaptureJSON: "{}", Occurrence: MediaOccurrenceInput{SourceID: sourceID, Principal: op.Principal,
						Ref: "recording", Revision: "1", Filename: "original.wav", MessageJSON: "{}"}}
				publish = func() (MediaPublicationReceipt, error) { return s.RetainSuppliedMedia(ctx, request) }
			} else {
				op.Verb = "import_recording_artifact"
				reference, _ := remoteStoreReference(t, s, sourceID,
					"00000000-0000-4000-8000-000000000090", "new-occurrence")
				request := remoteStoreArtifact(op.ID, sourceID, reference.OccurrenceID, "", testSHA256([]byte("new input")), hash, 17, physical)
				request.Operation, request.VirtualPath = op, virtualPath
				publish = func() (MediaPublicationReceipt, error) { return s.RetainRemoteRecordingMedia(ctx, request) }
				if operation == "input" {
					original := remoteStoreArtifact("00000000-0000-4000-8000-000000000092", sourceID,
						reference.OccurrenceID, "", testSHA256([]byte("original input")), testSHA256([]byte("original")), 17, physical)
					retained, err := s.RetainRemoteRecordingMedia(ctx, original)
					require.NoError(t, err)
					request.SourceVersionID, request.Kind, request.MediaType = retained.SourceVersionID, "transcript", "text/plain"
					publish = func() (MediaPublicationReceipt, error) { return s.ImportMediaInputArtifact(ctx, request) }
				}
			}
			counts := func() map[string]int {
				result := map[string]int{}
				for _, table := range []string{"nodes", "content_versions", "blobs", "ingests", "provenance", "media_sources", "media_source_versions", "media_occurrences", "media_input_artifacts", "media_operations"} {
					var count int
					require.NoError(t, s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
					result[table] = count
				}
				return result
			}
			before := counts()
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			role = pgx.Identifier{role}.Sanitize()
			// The receipt is the last write, after core content and media authority.
			_, err := setup.ExecContext(ctx, "REVOKE INSERT ON documents.media_operations FROM "+role)
			require.NoError(t, err)
			_, err = publish()
			require.ErrorContains(t, err, "permission denied for table media_operations")
			require.Equal(t, before, counts())
			_, err = s.NodeByPath(ctx, virtualPath)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.MediaOperationReceipt(ctx, op)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.media_operations TO "+role)
			require.NoError(t, err)
			receipt, err := publish()
			require.NoError(t, err)
			replayed, err := publish()
			require.NoError(t, err)
			require.Equal(t, receipt, replayed)
			node, err := s.NodeByPath(ctx, virtualPath)
			require.NoError(t, err)
			require.Equal(t, receipt.ContentVersionID, node.CurrentVersionID)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkMediaSnapshotsPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"source", "version", "sources", "occurrences"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			request := suppliedMediaPublicationFixture(t, s)
			retained, err := s.RetainSuppliedMedia(ctx, request)
			require.NoError(t, err)
			read := func(reader *Store) (any, error) {
				switch operation {
				case "source":
					return reader.MediaSource(ctx, request.Operation.Principal, retained.SourceID)
				case "version":
					return reader.MediaSourceVersion(ctx, request.Operation.Principal, retained.SourceID, retained.SourceVersionID)
				case "sources":
					items, total, more, err := reader.MediaSources(ctx, request.Operation.Principal, "", 10)
					return struct {
						Items []MediaSourceProjection
						Total int
						More  bool
					}{items, total, more}, err
				default:
					items, total, more, err := reader.MediaOccurrences(ctx, request.Operation.Principal, retained.SourceID, "", 10)
					return struct {
						Items []MediaOccurrenceProjection
						Total int
						More  bool
					}{items, total, more}, err
				}
			}
			before, err := read(s)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var cfg *pgx.ConnConfig
			err = conn.Raw(func(raw any) error {
				pg, ok := raw.(*stdlib.Conn)
				if !ok {
					return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
				}
				cfg = pg.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			var changed bool
			var mutationErr error
			cfg.Tracer = &afterFirstSelect{run: func() {
				// Change the source-wide receipt as well as visibility between statements.
				next := request
				next.Operation = testMediaOperation("submit_supplied_media", request.Operation.Principal)
				next.Operation.SourceID = request.SourceID
				_, mutationErr = s.RetainSuppliedMedia(ctx, next)
				if mutationErr == nil {
					mutationErr = revokeTestOccurrence(ctx, s, request.Operation.Principal, retained.OccurrenceID)
				}
				changed = true
			}}
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
			during, err := read(reader)
			require.NoError(t, mutationErr)
			require.True(t, changed)
			require.NoError(t, err)
			require.Equal(t, before, during)
			after, err := read(s)
			if operation == "source" || operation == "version" {
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.NoError(t, err)
				require.NotEqual(t, before, after, "the next request sees the committed revocation")
			}
		})
	}
}

func checkMediaIdentityConflictsPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	request := suppliedMediaPublicationFixture(t, s)
	retained, err := s.RetainSuppliedMedia(ctx, request)
	require.NoError(t, err)
	// PostgreSQL reports primary-key and UNIQUE conflicts with the same code.
	// These paths must use the Store classifier, not the SQLite driver.
	occurrence := request.Occurrence
	occurrence.ID, occurrence.SourceVersionID, occurrence.Revision = retained.OccurrenceID, retained.SourceVersionID, "2"
	_, err = s.RecordMediaOccurrence(ctx, testMediaOperation("declare_occurrence", occurrence.Principal), occurrence)
	require.ErrorIs(t, err, ErrMediaOccurrenceConflict)
	require.ErrorIs(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: retained.SourceVersionID, SourceID: retained.SourceID,
		Revision: 2, ExpectedHeadRevision: 1, ContentVersionID: retained.ContentVersionID, CaptureJSON: "{}",
	}), ErrMediaSourceConflict)
	view, err := s.MediaSource(ctx, request.Operation.Principal, retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, retained, view.Receipt)
	checkRetentionMetadata(t, s)
}

func checkMediaRevocationRemovesInputBoundRendition(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	principal := `operator\name`
	seedRenditionCatalogVersions(t, s)
	file, err := s.CreateFile(ctx, s.RootID(), "recording.wav", catalogSourceHash, 20, "audio/wav")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(ctx, file.CurrentVersionID)
	require.NoError(t, err)
	sourceID := testSHA256([]byte("rendition-media-source"))
	publication := MediaPublicationRequest{
		Operation: testMediaOperation("submit_supplied_media", principal),
		SourceID:  sourceID, ContentVersion: version, CaptureJSON: "{}",
		Occurrence: MediaOccurrenceInput{SourceID: sourceID, Principal: principal, Ref: "recording", Revision: "1", MessageJSON: "{}"},
	}
	publication.Operation.SourceID = sourceID
	retained, err := s.RetainSuppliedMedia(ctx, publication)
	require.NoError(t, err)
	input := MediaInputArtifactRequest{
		Operation: testMediaOperation("import_recording_artifact", publication.Operation.Principal),
		InputID:   testSHA256([]byte("selected-transcript")), OccurrenceID: retained.OccurrenceID,
		SourceVersionID: retained.SourceVersionID, VirtualPath: "/transcript.txt", MediaType: "text/plain", ByteLength: 10,
		Physical: BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true},
		Kind:     "transcript", Origin: "supplied", InputSHA: testSHA256([]byte("transcript")),
	}
	input.Operation.SourceID = sourceID
	_, err = s.ImportMediaInputArtifact(ctx, input)
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(version.ID, profile)
	request.Authorization.Principal = principal
	request.ExecutionIdentity.Upload.InputBinding = input.InputID
	request.ExecutionIdentity.Upload.MediaFamily, request.ExecutionIdentity.Authorization.MediaFamily = "audio", "audio"
	request.ExecutionIdentity.Upload.MediaType, request.ExecutionIdentity.Authorization.MediaType = "audio/wav", "audio/wav"
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "media-worker", at, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID, at, renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(ctx, claim, build, at))
	_, err = s.StageRenditionJobGeneration(ctx, claim, testSHA256([]byte("media-generation")), at)
	require.NoError(t, err)
	published, err := s.PublishRenditionJob(ctx, claim, at)
	require.NoError(t, err)
	require.Equal(t, 1, published.PublishedWaiterCount)
	_, err = s.ActiveRendition(ctx, version.ID, profile.Fingerprint)
	require.NoError(t, err)
	require.NoError(t, revokeTestOccurrence(ctx, s, publication.Operation.Principal, retained.OccurrenceID))
	_, err = s.ActiveRendition(ctx, version.ID, profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.RenditionBuild(ctx, job.ID)
	require.NoError(t, err, "revocation removes serving authority, not retained evidence")
	_, _, err = s.EnqueueRenditionJob(ctx, request)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
	checkRetentionMetadata(t, s)
}
