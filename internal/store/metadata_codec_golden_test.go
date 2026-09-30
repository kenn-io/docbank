package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/report"
)

const (
	metadataCodecGoldenPath           = "testdata/metadata-codec-golden.jsonl"
	metadataCodecGoldenTablesPath     = "testdata/metadata-codec-golden-tables.json"
	metadataCodecGoldenRejectionsPath = "testdata/metadata-codec-golden-rejections.json"
)

// metadataCodecGoldenKinds lists every backup record kind except the media
// kinds; the golden vault must contain each one.
var metadataCodecGoldenKinds = []string{
	"blob", "blob_checksum", "source_metadata_generation", "source_metadata_head",
	"visual_preview_generation", "visual_preview_head", "node", "content_version", "ingest",
	"provenance", "watch_source", "tag", "node_tag", "extracted_text", "collection_label",
	"provenance_version_binding", "saved_query", "saved_query_run", "term_report_history",
	"batch_tag_receipt", "email_generation", "email_part_artifact", "email_attachment", "email_head",
	"email_body_result", "email_document_publication", "mailbox_container", "mailbox_archive",
	"mailbox_transfer_receipt", "mailbox_transfer_head", "mailbox_job", "mailbox_occurrence",
	"page_document", "page_recipe", "page_image", "page_render_job", "export_authority",
	"collection_snapshot", "collection_snapshot_member", "collection_snapshot_representation",
	"package", "package_volume", "package_record", "package_label", "package_import_receipt",
	"package_import_head", "package_import_job", "bates_namespace", "bates_namespace_cursor",
	"bates_allocation", "bates_page_label", "bates_artifact", "bates_artifact_page",
	"photo_asset", "photo_file", "photo_library_settings", "photo_change_receipt",
	"processing_incarnation", "processing_consent_revocation", "processing_consent_grant",
	"processing_profile", "rendition_build", "rendition_artifact", "rendition_unit",
	"rendition_lexical_segment", "rendition_attachment", "rendition_head",
	"rendition_lexical_generation", "current_rendition_root", "derivative_purge_suppression",
	"rendition_job", "rendition_job_waiter",
	"embedding_vector_space", "embedding_input_generation", "embedding_generation_input",
	"embedding_vector_set", "embedding_vector_row", "embedding_set", "embedding_head", "embedding_failure",
	"person", "person_identity", "person_external_identity", "person_external_uid_alias",
	"person_alias", "person_merge", "person_split", "custodian_assignment",
	"person_document_assertion", "person_match_candidate",
	"audit_authority", "audit_scope", "audit_membership", "audit_record",
}

// metadataCodecGoldenTables leaves out blob_locations, whose generation is
// random on every import.
var metadataCodecGoldenTables = []string{
	"blobs", "blob_checksums", "source_metadata_generations", "source_metadata_heads",
	"visual_preview_generations", "visual_preview_heads", "nodes", "content_versions", "ingests",
	"provenance", "provenance_version_bindings", "watch_sources", "tags", "node_tags",
	"extracted_text", "content_fts", "collection_labels", "saved_queries", "saved_query_runs",
	"term_report_history", "batch_tag_receipts", "email_generations", "email_part_artifacts",
	"email_attachments", "email_heads", "email_body_results", "email_document_publications",
	"email_document_relations", "mailbox_containers", "mailbox_chunks", "mailbox_archives",
	"mailbox_transfer_receipts", "mailbox_transfer_heads", "mailbox_jobs", "mailbox_occurrences",
	"page_documents", "page_frames", "page_recipes", "page_images", "page_render_jobs",
	"export_sources", "export_members", "export_plans", "export_documents", "export_role_roots",
	"collection_snapshots", "collection_snapshot_members", "collection_snapshot_representations",
	"packages", "package_volumes", "package_records", "package_labels", "package_import_receipts",
	"package_import_heads", "package_import_jobs", "bates_namespaces", "bates_namespace_cursors",
	"bates_allocations", "bates_page_labels", "bates_artifacts", "bates_artifact_pages",
	"photo_assets", "photo_files", "photo_library_settings", "photo_change_receipts",
	"processing_incarnations", "processing_consent_revocations", "processing_consent_grants",
	"processing_profiles", "rendition_builds", "rendition_artifacts", "rendition_units",
	"rendition_lexical_segments", "rendition_attachments", "rendition_heads",
	"rendition_lexical_generations", "rendition_lexical_generation_builds",
	"rendition_lexical_generation_manifests", "rendition_lexical_heads", "current_rendition_roots",
	"derivative_purge_suppressions", "rendition_jobs", "rendition_job_waiters",
	"embedding_vector_spaces", "embedding_input_generations", "embedding_generation_inputs",
	"embedding_vector_sets", "embedding_vector_rows", "embedding_sets", "embedding_heads", "embedding_failures",
	"persons", "person_identities", "person_external_identities", "person_external_uid_aliases",
	"person_aliases", "person_merges", "person_splits", "custodian_assignments",
	"person_document_assertions", "person_match_candidates",
	"audit_authority", "audit_scopes", "audit_memberships", "audit_records", "audit_baselines",
}

// metadataCodecGoldenValidatorCases holds one mutation per kind whose import
// error comes from Go validation, not from SQLite. A dotted field names a
// member of a nested object.
var metadataCodecGoldenValidatorCases = map[string][2]string{
	"blob":                               {"size", `-1`},
	"blob_checksum":                      {"md5", `"synthetic-invalid"`},
	"source_metadata_generation":         {"contract_version", `"synthetic-invalid"`},
	"visual_preview_generation":          {"created_at", `"not-a-time"`},
	"visual_preview_head":                {"published_at", `"not-a-time"`},
	"node":                               {"revision", `0`},
	"content_version":                    {"transition_kind", `"synthetic-invalid"`},
	"ingest":                             {"started_at", `"not-a-time"`},
	"provenance":                         {"node_id", `0`},
	"watch_source":                       {"size", `-1`},
	"tag":                                {"revision", `0`},
	"node_tag":                           {"node_id", `0`},
	"extracted_text":                     {"status", `"synthetic-invalid"`},
	"collection_label":                   {"revision", `0`},
	"provenance_version_binding":         {"basis_ref", `"synthetic-invalid"`},
	"saved_query":                        {"revision", `0`},
	"saved_query_run":                    {"ran_at", `"not-a-time"`},
	"term_report_history":                {"observed_at", `"not-a-time"`},
	"batch_tag_receipt":                  {"operation_id", `"synthetic-invalid"`},
	"email_document_publication":         {"request.operation_id", `"synthetic-invalid"`},
	"mailbox_container":                  {"container.state", `"synthetic-invalid"`},
	"mailbox_archive":                    {"archive.owner", `""`},
	"mailbox_transfer_receipt":           {"receipt.request_digest", `"synthetic-invalid"`},
	"mailbox_job":                        {"job.state", `"synthetic-invalid"`},
	"mailbox_occurrence":                 {"occurrence.outcome", `"synthetic-invalid"`},
	"page_document":                      {"checksum", `"synthetic-invalid"`},
	"page_recipe":                        {"checksum", `"synthetic-invalid"`},
	"page_image":                         {"checksum", `"synthetic-invalid"`},
	"page_render_job":                    {"checksum", `"synthetic-invalid"`},
	"export_authority":                   {"checksum", `"synthetic-invalid"`},
	"collection_snapshot":                {"checksum", `"synthetic-invalid"`},
	"collection_snapshot_member":         {"checksum", `"synthetic-invalid"`},
	"collection_snapshot_representation": {"checksum", `"synthetic-invalid"`},
	"package":                            {"checksum", `"synthetic-invalid"`},
	"package_volume":                     {"checksum", `"synthetic-invalid"`},
	"package_record":                     {"checksum", `"synthetic-invalid"`},
	"package_label":                      {"checksum", `"synthetic-invalid"`},
	"package_import_receipt":             {"checksum", `"synthetic-invalid"`},
	"package_import_head":                {"checksum", `"synthetic-invalid"`},
	"package_import_job":                 {"checksum", `"synthetic-invalid"`},
	"bates_namespace":                    {"padding", `0`},
	"bates_namespace_cursor":             {"next_sequence", `0`},
	"bates_allocation":                   {"state", `"synthetic-invalid"`},
	"bates_page_label":                   {"ordinal", `0`},
	"bates_artifact":                     {"state", `"synthetic-invalid"`},
	"bates_artifact_page":                {"ordinal", `0`},
	"photo_asset":                        {"revision", `0`},
	"photo_file":                         {"role", `"synthetic-invalid"`},
	"photo_library_settings":             {"revision", `0`},
	"photo_change_receipt":               {"operation", `"synthetic-invalid"`},
	"processing_incarnation":             {"created_at", `"not-a-time"`},
	"processing_consent_revocation":      {"fence", `0`},
	"processing_consent_grant":           {"revocation_fence", `-1`},
	"processing_profile":                 {"trust_boundary", `"synthetic-invalid"`},
	"rendition_build":                    {"declared_artifact_count", `-1`},
	"rendition_artifact":                 {"state", `"synthetic-invalid"`},
	"rendition_unit":                     {"order", `-1`},
	"rendition_lexical_segment":          {"char_end", `-1`},
	"rendition_attachment":               {"attached_at", `"not-a-time"`},
	"rendition_head":                     {"published_at", `"not-a-time"`},
	"rendition_lexical_generation":       {"built_at", `"not-a-time"`},
	"current_rendition_root":             {"recorded_at", `"not-a-time"`},
	"derivative_purge_suppression":       {"purged_at", `"not-a-time"`},
	"rendition_job":                      {"state", `"synthetic-invalid"`},
	"rendition_job_waiter":               {"state", `"synthetic-invalid"`},
	"embedding_vector_space":             {"descriptor_json", `"e30="`},
	"embedding_input_generation":         {"input_count", `0`},
	"embedding_generation_input":         {"order", `-1`},
	"embedding_vector_set":               {"row_count", `0`},
	"embedding_vector_row":               {"order", `-1`},
	"embedding_set":                      {"created_at", `"not-a-time"`},
	"embedding_head":                     {"published_at", `"not-a-time"`},
	"embedding_failure":                  {"failed_at", `"not-a-time"`},
	"person":                             {"revision", `0`},
	"person_identity":                    {"confidence", `"synthetic-invalid"`},
	"person_external_identity":           {"uid_state", `"synthetic-invalid"`},
	"person_external_uid_alias":          {"observed_at", `"not-a-time"`},
	"person_alias":                       {"reason", `"synthetic-invalid"`},
	"person_merge":                       {"survivor_revision_after", `0`},
	"person_split":                       {"created_at", `"not-a-time"`},
	"custodian_assignment":               {"rank", `"synthetic-invalid"`},
	"person_document_assertion":          {"recorded_at", `"not-a-time"`},
	"person_match_candidate":             {"state", `"synthetic-invalid"`},
	"audit_authority":                    {"operation_sequence_high_water", `0`},
	"audit_scope":                        {"entry_count", `0`},
	"audit_membership":                   {"baseline_digest", `"synthetic-invalid"`},
	"audit_record":                       {"digest", `"synthetic-invalid"`},
}

// metadataCodecGoldenUnvalidatedKinds have no Go validator on import, so
// they get no validator mutation.
var metadataCodecGoldenUnvalidatedKinds = []string{
	"email_generation", "email_part_artifact", "email_attachment", "email_head", "email_body_result",
	"source_metadata_head", "mailbox_transfer_head",
}

func TestMetadataCodecGolden(t *testing.T) {
	t.Parallel()
	update := os.Getenv("UPDATE_GOLDEN") == "1"
	if update {
		source := seedMetadataCodecGoldenVault(t)
		var first bytes.Buffer
		require.NoError(t, source.ExportMetadata(t.Context(), &first))
		second := metadataCodecGoldenRoundTrip(t, first.Bytes())
		require.Equal(t, string(second), string(metadataCodecGoldenRoundTrip(t, second)),
			"the golden must be a fixpoint of import then export")
		require.NoError(t, os.WriteFile(metadataCodecGoldenPath, second, 0o600))
	}
	golden, err := os.ReadFile(metadataCodecGoldenPath)
	require.NoError(t, err)

	t.Run("export", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, string(golden), string(metadataCodecGoldenRoundTrip(t, golden)))
		kinds := map[string]bool{}
		for line := range bytes.SplitSeq(bytes.TrimSpace(golden), []byte{'\n'}) {
			kinds[metadataCodecGoldenLineType(t, line)] = true
		}
		for _, kind := range metadataCodecGoldenKinds {
			assert.True(t, kinds[kind], "golden lacks %s", kind)
		}
	})

	t.Run("tables", func(t *testing.T) {
		t.Parallel()
		target := newTestStore(t)
		require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(golden)))
		dump := map[string][]string{}
		for _, table := range metadataCodecGoldenTables {
			dump[table] = metadataCodecGoldenTableRows(t, target, table)
		}
		metadataCodecGoldenCompare(t, update, metadataCodecGoldenTablesPath, dump)
	})

	t.Run("rejections", func(t *testing.T) {
		t.Parallel()
		cases := metadataCodecGoldenRejectionCases(t, golden)
		names := make([]string, 0, len(cases))
		for name := range cases {
			names = append(names, name)
		}
		slices.Sort(names)
		results := make([]string, len(names))
		for index, name := range names {
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), bytes.NewReader(cases[name]))
			require.Error(t, err, name)
			results[index] = name + ": " + err.Error()
		}
		metadataCodecGoldenCompare(t, update, metadataCodecGoldenRejectionsPath, results)
	})
}

func TestMetadataCodecCorruptWaiterFailsExport(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, s, request)
	_, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(),
		`UPDATE rendition_job_waiters SET state='synthetic-invalid' WHERE waiter_id=?`, waiter.ID)
	require.NoError(t, err)

	var exported bytes.Buffer
	err = s.ExportMetadata(t.Context(), &exported)
	require.ErrorContains(t, err, "invalid rendition job waiter metadata")
	t.Logf("bytes written before failure: %d", exported.Len())
}

func seedMetadataCodecGoldenVault(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)

	// Embedding sets, heads and retention roots as in TestEmbeddingCatalogMetadataRoundTripsDeterministically.
	direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(ctx, direct))
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(ctx, chunk))
	for _, set := range []EmbeddingSetRecord{direct, chunk} {
		require.NoError(t, s.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{
			FencingToken: 1,
			Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: set.BindingID, InputKind: set.InputKind},
			SetID:        set.ID, VectorSpaceID: set.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	require.NoError(t, s.RecordEmbeddingFailure(ctx, EmbeddingFailureRecord{
		FencingToken:     1,
		ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
		BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
		FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
	}))
	for _, root := range []CurrentRenditionRoot{
		{ID: "golden-set", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingSet,
			TargetID: chunk.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "golden-released", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingVectorSet,
			TargetID: chunk.VectorSet.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
	} {
		require.NoError(t, s.PutCurrentRenditionRoot(ctx, root))
	}
	released, err := s.ReleaseCurrentRenditionRoot(ctx, "golden-released", 1)
	require.NoError(t, err)
	require.True(t, released)

	// Consent, a running job and its waiter as in TestProcessingMetadataClearsPublishedJobGenerationAfterCollection.
	request := renditionJobTestRequest(versionID, catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:golden", now, time.Hour)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	_, err = s.RevokeProcessingConsent(ctx, document.ProcessingConsentRevocationRequest{
		Principal: "operator:revoked", Scope: "document-processing",
	})
	require.NoError(t, err)

	// A purged plain-text version leaves a derivative purge suppression.
	purged, err := s.CreateFile(ctx, s.RootID(), "purged.txt", fakeHash("1f"), 5, "text/plain")
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{purged.CurrentVersionID}})
	require.NoError(t, err)

	seedMetadataCodecGoldenCore(t, s)
	seedMetadataCodecGoldenQueries(t, s)
	seedMetadataCodecGoldenEmail(t, s)
	seedMetadataCodecGoldenMailbox(t, s)
	seedMetadataCodecGoldenPagesAndBundles(t, s)
	seedMetadataCodecGoldenPackages(t, s)
	seedMetadataCodecGoldenBates(t, s)
	seedMetadataCodecGoldenPhotos(t, s)
	seedMetadataCodecGoldenPersons(t, s)

	audited, err := s.MkdirAll(ctx, "/Audited")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, audited.ID)
	return s
}

// seedMetadataCodecGoldenCore follows TestMetadataJSONLRoundTripPreservesLogicalState
// without its raw SQL, plus checksum, source metadata, visual preview, tag,
// extraction and collection label steps from their round-trip tests.
func seedMetadataCodecGoldenCore(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	label := "Synthetic Collection"
	run, err := s.BeginIngestWithLabel(ctx, "cli", "synthetic ingest", &label)
	require.NoError(t, err)
	ingested, added, err := s.IngestFile(ctx, run, s.RootID(), "ingested.txt", fakeHash("c01"), 8,
		"text/plain", "/source/ingested.txt", "2026-02-03T04:05:06.12Z")
	require.NoError(t, err)
	require.True(t, added)
	clearedLabel := "Cleared Collection"
	cleared, err := s.BeginIngestWithLabel(ctx, "cli", "cleared label", &clearedLabel)
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, cleared, s.RootID(), "cleared.txt", fakeHash("c02"), 8,
		"text/plain", "/source/cleared.txt", "")
	require.NoError(t, err)
	current := mustCollectionLabel(t, s, cleared.ID())
	_, err = s.SetCollectionLabel(ctx, cleared.ID(), current.Revision, nil)
	require.NoError(t, err)
	watchRun, err := s.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	_, err = s.IngestFileExact(ctx, watchRun, s.RootID(), "session.jsonl", fakeHash("c03"), 9,
		"application/json", "daily/session.jsonl", "")
	require.NoError(t, err)

	revised, err := s.CreateFile(ctx, s.RootID(), "revised.txt", fakeHash("c04"), 4, "text/plain")
	require.NoError(t, err)
	replaced, _, err := s.ReplaceContent(ctx, revised.ID, revised.Revision, fakeHash("c05"), 5, "text/plain")
	require.NoError(t, err)
	reverted, _, _, err := s.RevertContent(ctx, revised.ID, replaced.Revision, revised.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, revised.ID, reverted.ID)
	parent, err := s.Mkdir(ctx, s.RootID(), "trash-parent")
	require.NoError(t, err)
	lost, err := s.CreateFile(ctx, parent.ID, "lost.txt", fakeHash("c06"), 6, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, lost.ID, UnconditionalRev)
	require.NoError(t, err)

	tag, err := s.CreateTag(ctx, "important")
	require.NoError(t, err)
	_, err = s.AssignTagPath(ctx, tag.ID, "/ingested.txt")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: ingested.BlobHash,
		Extractor: "synthetic-extractor", ExtractorVersion: 1, Status: ExtractionOK, Text: "line one"}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: fakeHash("c05"),
		Extractor: "synthetic-extractor", ExtractorVersion: 1, Status: ExtractionFailed, Error: "synthetic failure"}))

	_, err = s.CreateFile(ctx, s.RootID(), "checksum.bin", fakeHash("c07"), 9, "application/octet-stream",
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true, Created: true, MD5: "f6fdffe48c908deb0f4c3bd36c032e72"})
	require.NoError(t, err)
	source, err := s.CreateFile(ctx, s.RootID(), "report.pdf", fakeHash("c08"), 4, "application/pdf")
	require.NoError(t, err)
	metadata, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("Synthetic title")}}}})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, source.BlobHash, fakeHash("c09"), metadata)
	require.NoError(t, err)
	image, err := s.CreateFile(ctx, s.RootID(), "image.jpg", fakeHash("c10"), 12, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(ctx, image.CurrentVersionID, readyVisualPreview(t, image.BlobHash, fakeHash("c11"), 7),
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 7})
	require.NoError(t, err)
}

// seedMetadataCodecGoldenQueries follows the saved-query run, term report and
// batch-tag round-trip tests.
func seedMetadataCodecGoldenQueries(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	definition, err := s.CreateSavedQuery(ctx, "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{
		HMACKey: bytes.Repeat([]byte{0x58}, 32), Now: func() time.Time { return now },
	})
	for range 2 {
		_, _, err = service.RunSaved(ctx, "owner", definition.ID, definition.Revision, SnapshotRequest{}, nil)
		require.NoError(t, err)
	}
	require.NoError(t, service.Close())

	observed := time.Date(2026, 9, 20, 15, 30, 0, 0, time.UTC)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}}}}
	require.NoError(t, s.SaveTermReportHistory(ctx, TermReportHistory{Request: request, Summary: report.Summary{
		ID: strings.Repeat("a", 48), State: report.StateComplete, ObservedAt: observed,
		ExpiresAt: observed.Add(30 * time.Minute), Terms: request.Terms, Counts: []report.Counts{{Hits: 2}},
	}}))

	selected, err := s.Mkdir(ctx, s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	_, err = s.BatchTags(ctx, BatchTagRequest{
		OperationID: "11111111-1111-4111-8111-111111111111", TagID: tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: selected.ID, Revision: selected.Revision}},
	})
	require.NoError(t, err)
}

// seedMetadataCodecGoldenEmail follows the email lifecycle and email document
// publication tests.
func seedMetadataCodecGoldenEmail(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	fixture := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(ctx, fixture.publication)
	require.NoError(t, err)
	bodyRecipe, err := document.EmailBodyRecipeFingerprint(view.Evidence.Recipe)
	require.NoError(t, err)
	require.NoError(t, s.RecordEmailBodyUnavailable(ctx, view.Attachment.ID, bodyRecipe, new("1.1"), "empty_body"))
	_, err = s.PublishEmailDocuments(ctx, attachmentRequest(t, s, view, "portable-receipt"))
	require.NoError(t, err)
}

// seedMetadataCodecGoldenMailbox follows TestMailboxTransferAtomicPublicationAndConcurrentRetry
// and TestMailboxJobClaimCheckpointCancellationAndContinuation.
func seedMetadataCodecGoldenMailbox(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	fixture := newEmailFixture(t, s, "seed.eml")
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "external-synthetic", Owner: "one", Description: "Synthetic exported EML"}))
	run, err := s.BeginIngest(ctx, "mailbox", "Synthetic EML transfer")
	require.NoError(t, err)
	source, err := emailVersion(ctx, s.db, fixture.publication.ContentVersionID)
	require.NoError(t, err)
	_, err = s.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Email: fixture.publication,
		Request: MailboxTransferRequest{ArchiveID: "external-synthetic", Reference: "message-1", SHA256: source.BlobHash,
			Size: source.Size, Settings: "settings-v1", DestinationID: s.RootID(), Name: "message.eml"}})
	require.NoError(t, err)

	container := MailboxContainerRequest{ID: "source", Owner: "one", SHA256: fakeHash("c12"), Size: 3, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, container)
	require.NoError(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, container.SHA256, 3, BlobPhysical{Encoding: "raw", StoredBytes: 3, PackEligible: true, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, "one", "source", MailboxChunk{Index: 0, SHA256: container.SHA256, Size: 3}))
	_, err = s.SealMailboxContainer(ctx, "one", "source", container.SHA256, 3)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, "one", MailboxJobRequest{ID: "job", ContainerID: "source", ContainerSHA256: container.SHA256,
		Settings: MailboxSettings{Dialect: "mboxrd", DestinationID: s.RootID()}})
	require.NoError(t, err)
	claimed, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.CommitMailboxOccurrence(ctx, claimed.ID, claimed.Claim, MailboxOccurrence{JobID: claimed.ID, Ordinal: 1,
		Outcome: "rejected", Reason: "synthetic malformed message", Location: MailboxLocation{ContainerID: "source",
			Entry: "source.mbox", EntrySHA256: container.SHA256, Sequence: 1, Start: 0, End: 3,
			Separator: "From synthetic", RawSHA256: container.SHA256, Labels: []string{}}}, nil))
	require.NoError(t, s.FinishMailboxJob(ctx, claimed.ID, claimed.Claim, "partial", "segment limit", false))
}

// seedMetadataCodecGoldenPagesAndBundles follows TestPagePublicationFencesClaimsAndPreservesPartialAuthority
// and TestExportJobsFenceRestartCancelAndPortableAuthority.
func seedMetadataCodecGoldenPagesAndBundles(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	request := pageStoreRequest(t, s)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.QueuePageJob(ctx, operation, request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(ctx)
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(ctx, claim, frames))
	recipe := pageStoreRecipe()
	require.NoError(t, s.PublishPageImage(ctx, claim, pageStoreImage(t, frames[0], recipe), recipe,
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))

	node, err := s.CreateFile(ctx, s.RootID(), "bundle.txt", fakeHash("c13"), 12, "text/plain")
	require.NoError(t, err)
	sourceOperation, err := newUUIDv4()
	require.NoError(t, err)
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: sourceOperation, Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}}}, nil)
	require.NoError(t, err)
	planOperation, err := newUUIDv4()
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(ctx, "owner", bundle.PlanRequest{OperationID: planOperation, SourceID: source.ID,
		MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	jobOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.QueueExportJob(ctx, "owner", bundle.JobRequest{OperationID: jobOperation, PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
}

// seedMetadataCodecGoldenPackages follows TestPackageImportReceiptsAndLabelsSurviveMetadataRestore
// and seals a snapshot with a representation as TestBatesExplicitPagesMatchASealedPDFWithoutPageDocument does.
func seedMetadataCodecGoldenPackages(t *testing.T, s *Store) {
	t.Helper()
	pkg, node := seedReceivedPackage(t, s, "portable")
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "EXT000001")
	native, err := s.CreateFile(t.Context(), s.RootID(), "native.pdf", fakeHash("c14"), 123, "application/pdf")
	require.NoError(t, err)
	occurrence := strings.Repeat("d", 32)
	id, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: native.ID, ContentVersionID: native.CurrentVersionID,
		BlobSHA256: native.BlobHash, Size: 123, FamilyID: occurrence, FamilyOrder: 1, DisplayName: native.Name,
		FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: 2, SelectedPDFSHA256: native.BlobHash,
		Representations: []CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native",
			Status: roleAvailable, TextAuthority: "none", ContentVersionID: native.CurrentVersionID,
			BlobSHA256: native.BlobHash, MediaType: "application/pdf", Size: 123, VerifiedPageCount: 2}},
	}}})
	require.NoError(t, err)
}

// seedMetadataCodecGoldenBates follows TestBatesArtifactReceiptsMustNameTheSealedSourcePDF
// with matching pages, then leaves a second allocation reserved.
func seedMetadataCodecGoldenBates(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	snapshot, inputs := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(ctx, "SRC", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	request := batesRequest(t, namespace, snapshot, inputs)
	request.RecipeSHA256 = digestCatalogJSON(recipe)
	allocation, err := s.ReserveBatesRange(ctx, request)
	require.NoError(t, err)
	pages := make([]BatesArtifactPage, len(inputs))
	for index, input := range inputs {
		pages[index] = BatesArtifactPage{Ordinal: index + 1, OccurrenceID: input.OccurrenceID,
			SourceBlobSHA256: input.UnstampedSHA256, SourcePage: input.SourcePage,
			OutputPage: index + 1, Label: allocation.Labels[index].Label}
	}
	_, err = s.PublishBatesArtifact(ctx, BatesArtifactPublication{ArtifactID: allocation.AllocationID,
		AllocationID: allocation.AllocationID, BlobSHA256: fakeHash("c15"), Size: 10,
		PageCount: len(pages), RecipeJSON: recipe, Pages: pages}, BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	require.NoError(t, err)
	reserved := batesRequest(t, namespace, snapshot, inputs)
	reserved.StartAt = int64(len(inputs)) + 1
	_, err = s.ReserveBatesRange(ctx, reserved)
	require.NoError(t, err)
}

// seedMetadataCodecGoldenPhotos follows TestPhotoAssetGroupsRawJPEGSidecar and
// the photo settings test.
func seedMetadataCodecGoldenPhotos(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("c16"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpeg, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("c17"), 4, "image/jpeg")
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c18"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawFile.ID)
	require.NoError(t, err)
	preference := "image"
	_, err = s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
}

// seedMetadataCodecGoldenPersons follows TestPersonMetadataRoundTrip and adds a
// deleted person so one alias has a null survivor.
func seedMetadataCodecGoldenPersons(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	identity, err := s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{
		Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "identity-1", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "person-current", []string{"person-retired"}))
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{
		PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic", UID: "person-current",
		UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: person.DisplayName,
	}, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	version := seedDocumentPeopleEvent(t, s, "metadata.txt", "a9", nil)
	_, err = s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: version.NodeID, ContentVersionID: version.ID},
		PersonID: person.PersonID, RawLabel: person.DisplayName, Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic", IfMatchRevision: 1})
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID,
		PersonID: person.PersonID, Role: "author", Action: "assert", Note: "synthetic note", Revision: 1})
	require.NoError(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: version.ID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim-1"}})
	require.NoError(t, err)
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, _, err := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "name_alias:ada lovelace",
			DisplayName: person.DisplayName, SuggestedPersonID: person.PersonID, Reason: "name_only", Evidence: evidence})
		return err
	}))
	absorbed, err := s.CreatePerson(ctx, "Grace Hopper", "operator")
	require.NoError(t, err)
	mergeOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, person.PersonID, absorbed.PersonID, mergeOperation, person.Revision, absorbed.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	splitOperation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.SplitPerson(ctx, PersonSplitRequest{PersonID: person.PersonID, OperationID: splitOperation,
		DisplayName: "Ada Byron", Revision: person.Revision, IdentityIDs: []string{identity.IdentityID}})
	require.NoError(t, err)
	deleted, err := s.CreatePerson(ctx, "Retired Example", "operator")
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, deleted.PersonID, deleted.Revision)
	require.NoError(t, err)
}

func metadataCodecGoldenRoundTrip(t *testing.T, input []byte) []byte {
	t.Helper()
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(input)))
	var exported bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &exported))
	return exported.Bytes()
}

func metadataCodecGoldenLineType(t *testing.T, line []byte) string {
	t.Helper()
	var header struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(line, &header))
	return header.Type
}

func metadataCodecGoldenTableRows(t *testing.T, s *Store, table string) []string {
	t.Helper()
	var columns, selects []string
	func() {
		columnRows, err := s.db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		require.NoError(t, err)
		defer func() { require.NoError(t, columnRows.Close()) }()
		for columnRows.Next() {
			var column string
			require.NoError(t, columnRows.Scan(&column))
			columns = append(columns, column)
			selects = append(selects, "typeof("+column+"),quote("+column+")")
		}
		require.NoError(t, columnRows.Err())
	}()
	require.NotEmpty(t, columns, table)
	var filter string
	if table == "processing_incarnations" {
		// The import target creates its own random current incarnation.
		filter = ` WHERE incarnation_id NOT IN (SELECT incarnation_id FROM current_processing_incarnation)`
	}
	rows, err := s.db.QueryContext(t.Context(), `SELECT `+strings.Join(selects, ",")+` FROM `+table+filter)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []string
	for rows.Next() {
		values := make([]string, 2*len(columns))
		targets := make([]any, len(values))
		for index := range values {
			targets[index] = &values[index]
		}
		require.NoError(t, rows.Scan(targets...))
		cells := make([]string, len(columns))
		for index, column := range columns {
			cells[index] = column + ":" + values[2*index] + ":" + values[2*index+1]
		}
		result = append(result, strings.Join(cells, " | "))
	}
	require.NoError(t, rows.Err())
	slices.Sort(result)
	return result
}

type metadataCodecGoldenMember struct {
	name  string
	value jsontext.Value
}

func metadataCodecGoldenRejectionCases(t *testing.T, golden []byte) map[string][]byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(golden), []byte{'\n'})
	cases := map[string][]byte{}
	seen := map[string]bool{}
	for index, line := range lines {
		kind := metadataCodecGoldenLineType(t, line)
		if !slices.Contains(metadataCodecGoldenKinds, kind) || seen[kind] {
			continue
		}
		seen[kind] = true
		members := metadataCodecGoldenMembers(t, line)
		mutate := func(name string, changed []metadataCodecGoldenMember) {
			mutated := slices.Clone(lines)
			mutated[index] = metadataCodecGoldenEncode(t, changed)
			cases[kind+"/"+name] = append(bytes.Join(mutated, []byte{'\n'}), '\n')
		}
		mutate("unknown-field", append(slices.Clone(members), metadataCodecGoldenMember{"x_unknown", jsontext.Value(`1`)}))
		mutate("missing-last-field", slices.Clone(members[:len(members)-1]))
		nulled := slices.Clone(members)
		first := slices.IndexFunc(nulled[1:], func(member metadataCodecGoldenMember) bool {
			return !metadataNullableFields[kind][member.name]
		})
		require.GreaterOrEqual(t, first, 0, kind)
		nulled[first+1].value = jsontext.Value(`null`)
		mutate("null-first-field", nulled)
		if slices.Contains(metadataCodecGoldenUnvalidatedKinds, kind) {
			continue
		}
		validator, ok := metadataCodecGoldenValidatorCases[kind]
		require.True(t, ok, kind)
		mutate("validator-"+validator[0], metadataCodecGoldenSet(t, members, strings.Split(validator[0], "."), validator[1]))
	}
	return cases
}

// metadataCodecGoldenSet replaces the member at path, descending into nested
// objects for each earlier path element.
func metadataCodecGoldenSet(t *testing.T, members []metadataCodecGoldenMember, path []string, value string) []metadataCodecGoldenMember {
	t.Helper()
	changed := slices.Clone(members)
	position := slices.IndexFunc(changed, func(member metadataCodecGoldenMember) bool { return member.name == path[0] })
	require.GreaterOrEqual(t, position, 0, "record lacks %s", path[0])
	if len(path) == 1 {
		changed[position].value = jsontext.Value(value)
		return changed
	}
	nested := metadataCodecGoldenSet(t, metadataCodecGoldenMembers(t, changed[position].value), path[1:], value)
	changed[position].value = metadataCodecGoldenEncode(t, nested)
	return changed
}

func metadataCodecGoldenMembers(t *testing.T, line []byte) []metadataCodecGoldenMember {
	t.Helper()
	decoder := jsontext.NewDecoder(bytes.NewReader(line))
	_, err := decoder.ReadToken()
	require.NoError(t, err)
	var members []metadataCodecGoldenMember
	for decoder.PeekKind() != '}' {
		token, err := decoder.ReadToken()
		require.NoError(t, err)
		name := token.String()
		value, err := decoder.ReadValue()
		require.NoError(t, err)
		members = append(members, metadataCodecGoldenMember{name, value.Clone()})
	}
	return members
}

func metadataCodecGoldenEncode(t *testing.T, members []metadataCodecGoldenMember) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := jsontext.NewEncoder(&output)
	require.NoError(t, encoder.WriteToken(jsontext.BeginObject))
	for _, member := range members {
		require.NoError(t, encoder.WriteToken(jsontext.String(member.name)))
		require.NoError(t, encoder.WriteValue(member.value))
	}
	require.NoError(t, encoder.WriteToken(jsontext.EndObject))
	return bytes.TrimSpace(output.Bytes())
}

func metadataCodecGoldenCompare(t *testing.T, update bool, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value, jsontext.Multiline(true), json.Deterministic(true))
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	if update {
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
	}
	expected, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(expected), string(encoded), path+" differs")
}
