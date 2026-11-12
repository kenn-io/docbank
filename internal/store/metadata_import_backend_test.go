package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkMetadataImportCatalogsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	seeds := retainedMetadataCatalogSeeds()
	seeds["mailbox work"] = seedMetadataImportMailbox
	seeds["mailbox job"] = func(t *testing.T) *Store {
		t.Helper()
		s := seeds["mailbox"](t)
		_, err := s.BeginMailboxJob(t.Context(), "operator", MailboxJobRequest{ID: "queued-job", ContainerID: "synthetic-mailbox", ContainerSHA256: fakeHash("ac"), Settings: MailboxSettings{DestinationID: s.RootID()}})
		require.NoError(t, err)
		return s
	}
	seeds["photo suggestions"] = seedMetadataImportSuggestions
	seeds["rendered pages"] = seedMetadataImportPages
	seeds["received records"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		job, record, labels, receipt := receivedRecordPublication(t, s)
		_, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
		require.NoError(t, err)
		_, err = s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, "failed", "", nil)
		require.NoError(t, err)
		return s
	}
	seeds["export plan"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		plan := exportPlanFixture(t, s, "operator")
		id, err := newUUIDv4()
		require.NoError(t, err)
		job, err := s.QueueExportJob(t.Context(), "operator", bundle.JobRequest{OperationID: id, PlanID: plan.ID, Fingerprint: plan.Fingerprint})
		require.NoError(t, err)
		claim, err := s.ClaimExportJob(t.Context())
		require.NoError(t, err)
		require.NoError(t, s.AdvanceExportJob(t.Context(), claim, plan.RoleEntries, plan.RoleBytes))
		require.NoError(t, s.FinishExportJob(t.Context(), claim, &bundle.Receipt{Format: bundle.Format, PlanFingerprint: plan.Fingerprint, SHA256: fakeHash("e2"), Size: 100, Entries: plan.ArchiveEntries()}, job.ID+".zip", ""))
		return s
	}
	seeds["represented snapshot"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "report.txt", fakeHash("a3"), 12, "text/plain")
		require.NoError(t, err)
		id, err := newUUIDv4()
		require.NoError(t, err)
		occurrence := strings.Repeat("c", 32)
		_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: node.Size, FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other", Representations: []CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native", Status: roleAvailable, TextAuthority: "none", ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, MediaType: node.MimeType, Size: node.Size}}}}})
		require.NoError(t, err)
		return s
	}
	seeds["media input"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		retained, err := s.RetainSuppliedMedia(t.Context(), suppliedMediaPublicationFixture(t, s))
		require.NoError(t, err)
		_, err = s.ImportMediaInputArtifact(t.Context(), MediaInputArtifactRequest{
			Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000064", Principal: "operator", Verb: "import_recording_artifact", SourceID: retained.SourceID, RequestSHA256: testSHA256([]byte("artifact-request"))},
			InputID:   testSHA256([]byte("artifact")), OccurrenceID: retained.OccurrenceID, SourceVersionID: retained.SourceVersionID,
			VirtualPath: "/media-input/transcript.txt", MediaType: "text/plain", ByteLength: 20,
			Physical: BlobPhysical{Encoding: "raw", StoredBytes: 20, Created: true}, Kind: "transcript", Origin: "supplied", Language: "en", InputSHA: testSHA256([]byte("transcript")),
		})
		require.NoError(t, err)
		return s
	}
	seeds["preview and checksum"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "photo.raw", fakeHash("11"), 12, "image/x-raw")
		require.NoError(t, err)
		_, err = s.PublishVisualPreview(t.Context(), node.CurrentVersionID, readyVisualPreview(t, node.BlobHash, fakeHash("22"), 9), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		require.NoError(t, s.RecordVerifiedBlobChecksum(t.Context(), BlobChecksumRecord{BlobSHA256: node.BlobHash, MD5: strings.Repeat("a", 32)}))
		return s
	}
	seeds["audit and moved parents"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		seedMetadataRoundTrip(t, s)
		parent, err := s.Mkdir(t.Context(), s.RootID(), "newer-parent")
		require.NoError(t, err)
		old, err := s.NodeByPath(t.Context(), "/Projects")
		require.NoError(t, err)
		_, _, err = s.Move(t.Context(), old.ID, parent.ID, "Projects", old.Revision)
		require.NoError(t, err)
		deleted, err := s.Mkdir(t.Context(), s.RootID(), "deleted-highest")
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), deleted.ID, deleted.Revision)
		require.NoError(t, err)
		_, err = s.TrashEmptyBounded(t.Context(), 0, 10, true)
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		return s
	}
	seeds["hidden photo lockout"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		asset := albumAsset(t, s, "hidden.jpg")
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
		require.NoError(t, err)
		for range 4 {
			_, _, err := s.UnlockPhotoHidden(t.Context(), "wrong-passcode")
			require.Error(t, err)
		}
		return s
	}
	seeds["processing jobs"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		versions := seedRenditionCatalogVersions(t, s)
		request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
		grantRenditionJobConsent(t, s, request)
		_, _, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		return s
	}
	seeds["candidate faces"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, face := faceForDecisionTest(t, s, "")
		buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, 1, 0)
		person, err := s.CreatePerson(t.Context(), "Example Person", "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
		require.NoError(t, err)
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		return s
	}
	seeds["photo projection pages"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "Synthetic dated photos")
		require.NoError(t, err)
		tag, err := s.CreateTag(t.Context(), "retained")
		require.NoError(t, err)
		for i := range 205 {
			node := ingestMtimePhoto(t, s, run, fmt.Sprintf("photo-%03d.jpg", i), "2024-03-05T10:00:00Z")
			_, err := s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
		}
		return s
	}
	// Verify these native fixtures actually produce the custom records whose
	// insertion behavior the round trip must exercise.
	requiredKinds := map[string][]string{
		"mailbox job":             {"mailbox_job"},
		"mailbox work":            {"mailbox_job", "mailbox_occurrence", "mailbox_archive", "mailbox_transfer_receipt", "mailbox_container", "email_document_publication"},
		"photo suggestions":       {"photo_suggestion"},
		"rendered pages":          {"page_document", "page_recipe", "page_image", "page_render_job"},
		"preview and checksum":    {"blob_checksum", "visual_preview_generation"},
		"received records":        {"package_record", "package_label", "package_import_receipt", "package_import_job", "package", "package_volume"},
		"represented snapshot":    {"collection_snapshot", "collection_snapshot_member", "collection_snapshot_representation"},
		"export plan":             {"export_authority"},
		"candidate faces":         {"person_document_assertion"},
		"media input":             {"media_source", "media_source_version", "media_occurrence", "media_input_artifact", "media_operation"},
		"embeddings":              {"embedding_input_generation"},
		"photo checkout":          {"photo_checkout", "photo_checkout_entry"},
		"audit and moved parents": {"blob", "audit_record"},
		"hidden photo lockout":    {"photo_hidden_failure"},
		"processing":              {"processing_profile", "rendition_head", "rendition_lexical_generation", "current_rendition_root"},
		"processing jobs":         {"processing_incarnation", "processing_consent_grant", "rendition_job", "rendition_job_waiter"},
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			source := seed(t)
			var input bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &input))
			require.Subset(t, metadataImportRecordTypes(t, input.Bytes()), requiredKinds[name])
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
			runtime := newPostgresSchemaStore(t, setup, cfg)
			var output bytes.Buffer
			require.NoError(t, runtime.ExportMetadata(ctx, &output))
			require.Equal(t, input.String(), output.String())
			require.NoError(t, runtime.ValidateMetadata(ctx))
			recovered := newTestStore(t)
			require.NoError(t, recovered.ImportMetadata(ctx, bytes.NewReader(output.Bytes())))
			var roundTrip bytes.Buffer
			require.NoError(t, recovered.ExportMetadata(ctx, &roundTrip))
			require.Equal(t, input.String(), roundTrip.String())
			projections := func(store *Store) []string {
				var result []string
				require.NoError(t, queryRows(ctx, store.db, `SELECT file_id,sort_key,day,COALESCE(modified_time,''),COALESCE(folder_key,'') FROM photo_timeline ORDER BY file_id`, nil, func(rows *sql.Rows) error {
					var file, sortKey, day, modified, folder string
					if err := rows.Scan(&file, &sortKey, &day, &modified, &folder); err != nil {
						return err
					}
					result = append(result, file, sortKey, day, modified, folder)
					return nil
				}))
				require.NoError(t, queryRows(ctx, store.db, `SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, nil, func(rows *sql.Rows) error {
					var asset string
					if err := rows.Scan(&asset); err != nil {
						return err
					}
					result = append(result, asset)
					return nil
				}))
				return result
			}
			require.Equal(t, projections(recovered), projections(runtime))
			if name == "photo projection pages" {
				for _, store := range []*Store{source, runtime, recovered} {
					var timeline, pending int
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_timeline`).Scan(&timeline))
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&pending))
					require.Equal(t, 205, timeline)
					require.Equal(t, 205, pending)
				}
			}

			before, err := source.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			after, err := runtime.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, before, after)
			if name == "hidden photo lockout" {
				_, _, err := runtime.UnlockPhotoHidden(ctx, "wrong-passcode")
				require.Error(t, err)
				var count, lastID int64
				require.NoError(t, runtime.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(failure_id) FROM photo_hidden_failures`).Scan(&count, &lastID))
				require.Equal(t, int64(5), count)
				require.Equal(t, int64(5), lastID)
			}
			if name == "candidate faces" {
				visible, _, err := runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Len(t, visible, 1)
				node, err := runtime.NodeByPath(ctx, "/portrait.jpg")
				require.NoError(t, err)
				asset, err := runtime.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				_, err = runtime.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				visible, _, err = runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Empty(t, visible, "restored candidate face membership must hide evidence from hidden photos")
			}
			if name == "audit and moved parents" {
				sequence, err := source.auditNodeSequence(ctx, source.db)
				require.NoError(t, err)
				node, err := runtime.Mkdir(ctx, runtime.RootID(), "after-transfer")
				require.NoError(t, err)
				require.Equal(t, sequence+1, node.ID)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
		})
	}
}

func metadataImportRecordTypes(t *testing.T, input []byte) []string {
	t.Helper()
	var kinds []string
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var record struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &record))
		kinds = append(kinds, record.Type)
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

func seedMetadataImportMailbox(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	s := newTestStore(t)
	email := newEmailFixture(t, s, "seed.eml")
	source, err := emailVersion(ctx, s.db, email.publication.ContentVersionID)
	require.NoError(t, err)
	c := MailboxContainerRequest{ID: "source", Owner: "operator", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	job, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "mailbox:" + job.CollectionID, Owner: c.Owner, Description: "Synthetic archive"}))
	settings, err := job.Settings.Canonical()
	require.NoError(t, err)
	location := MailboxLocation{ContainerID: c.ID, Entry: "source.mbox", EntrySHA256: c.SHA256, Sequence: 1, End: c.Size, RawSHA256: c.SHA256, EMLSHA256: c.SHA256, EMLSize: c.Size, Separator: "From synthetic", Labels: []string{}}
	occurrence := MailboxOccurrence{JobID: job.ID, Ordinal: 1, Location: location}
	require.NoError(t, s.StartMailboxOccurrence(ctx, job.ID, job.Claim, occurrence))
	publication := MailboxTransferPublication{Owner: c.Owner, Request: MailboxTransferRequest{ArchiveID: "mailbox:" + job.CollectionID, Reference: "0:1", SHA256: c.SHA256, Size: c.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: email.publication, Location: &location}
	require.NoError(t, s.CommitMailboxOccurrence(ctx, job.ID, job.Claim, occurrence, &publication))
	require.NoError(t, s.FinishMailboxJob(ctx, job.ID, job.Claim, "complete", "", true))
	return s
}

func seedMetadataImportSuggestions(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	asset := albumAsset(t, s, "partly-decided.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}})
	acceptPhotoSuggestion(t, s, proposal, true, "coast")
	asset = albumAsset(t, s, "dismissed.jpg")
	proposal = putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A river."})
	_, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, dismissalOf(t.Context(), t, s, proposal))
	require.NoError(t, err)
	asset = albumAsset(t, s, "settled.jpg")
	proposal = putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A mountain."})
	acceptPhotoSuggestion(t, s, proposal, true)
	return s
}

func seedMetadataImportPages(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	id, err := newUUIDv4()
	require.NoError(t, err)
	job, err := s.QueuePageJob(t.Context(), id, request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	recipe := pageStoreRecipe()
	require.NoError(t, s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frames[0], recipe), recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))
	require.NoError(t, s.CancelPageJob(t.Context(), job.ID, request.Binding()))
	return s
}

type failedMetadataReader struct{ err error }

func (r failedMetadataReader) Read([]byte) (int, error) { return 0, r.err }

func checkMetadataImportRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	_, err := source.CreateFile(t.Context(), source.RootID(), "retained.txt", fakeHash("a1"), 9, "text/plain")
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &input))
	for name, late := range map[string]bool{"interrupted stream": false, "late allocation write": true} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			before, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			var broken = io.MultiReader(bytes.NewReader(input.Bytes()), failedMetadataReader{errors.New("source interrupted")})
			writer := setup
			injected := false
			var injectionErr error
			if late {
				conn, err := setup.Conn(ctx)
				require.NoError(t, err)
				var config *pgx.ConnConfig
				require.NoError(t, conn.Raw(func(raw any) error {
					native, ok := raw.(*stdlib.Conn)
					require.True(t, ok)
					config = native.Conn().Config().Copy()
					return nil
				}))
				require.NoError(t, conn.Close())
				config.Tracer = renditionQueryStart(func(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) {
					if !injected && strings.HasPrefix(data.SQL, "UPDATE node_allocation SET high_water=") {
						injectionErr = setup.QueryRowContext(ctx, `SELECT pg_terminate_backend($1,5000)`, conn.PgConn().PID()).Scan(&injected)
					}
				})
				writer = stdlib.OpenDB(*config)
				t.Cleanup(func() { require.NoError(t, writer.Close()) })
				broken = bytes.NewReader(input.Bytes())
			}
			require.Error(t, ImportPostgresMetadata(ctx, writer, cfg, broken))
			if late {
				require.NoError(t, injectionErr)
				require.True(t, injected)
			}

			after, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			require.Equal(t, before, after)
			var nodes, blobs, highWater int64
			require.NoError(t, setup.QueryRowContext(ctx, `SELECT
				(SELECT COUNT(*) FROM documents.nodes), (SELECT COUNT(*) FROM documents.blobs),
				(SELECT high_water FROM documents.node_allocation WHERE singleton=1)`).Scan(&nodes, &blobs, &highWater))
			require.Equal(t, int64(1), nodes)
			require.Zero(t, blobs)
			require.Equal(t, int64(1), highWater)

			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
		})
	}
}

func checkMetadataImportAdmissionPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	setup.SetMaxOpenConns(1)
	var originalPath string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&originalPath))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	require.ErrorContains(t, ImportPostgresMetadata(ctx, runtime.db, cfg, bytes.NewReader(input.Bytes())), "schema owner role")
	_, err := runtime.Mkdir(ctx, runtime.RootID(), "keep-this")
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, runtime.ExportMetadata(ctx, &before))
	require.ErrorContains(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())), "not pristine")
	require.NoError(t, runtime.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	var path string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&path))
	require.Equal(t, originalPath, path, "the caller's pool search path stays unchanged")
	require.Zero(t, setup.Stats().InUse)
}

func checkMetadataImportKeepsFreshConsentPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	request := testProviderAuthorizationRequest()
	_, err := source.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = source.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	_, err = runtime.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
}
