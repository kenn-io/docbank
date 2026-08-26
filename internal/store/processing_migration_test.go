package store

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const legacyMigrationTimestamp = "2026-08-22T12:00:00.000000000Z"

// Mutation caught: hashing normalized text, omitting one legacy identity
// field, or changing the domain separator silently aliases a different build.
func TestLegacyPlainTextBuildFingerprintUsesExactStoredBytes(t *testing.T) {
	text := []byte("Cafe\u0301\r\nline\rtrail")
	assert.Equal(t,
		"10fb8bb322c770ba112a9a5bb5438369705cfccd95b43a301db1b0005d3833b2",
		legacyPlainTextBuildFingerprint(fakeHash("61"), "plain-text", 1, "ok", text),
	)
}

// Mutations caught: broadening the eligible extractor row, building once per
// version instead of per blob, normalizing stored bytes, leaving legacy FTS
// serving, or failing to queue every repairable selected blob without an eligible row.
func TestMigrateLegacyPlainTextCutsOverExactEligibleRows(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	exactText := "Cafe\u0301\r\nline\rtrail migration-compatible"
	eligibleHash := fakeHash("61")
	first, err := s.CreateFile(ctx, s.RootID(), "report-eligible.txt", eligibleHash, int64(len(exactText)), "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "shared-eligible.txt", eligibleHash, int64(len(exactText)), "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: eligibleHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: exactText,
	}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: eligibleHash, Extractor: "other", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "same-blob-competitor",
	}))

	failedHash := seedLegacyMigrationRow(t, s, "failed.txt", "62", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionFailed, Error: "synthetic failure",
	})
	unknownHash := seedLegacyMigrationRow(t, s, "unknown.txt", "63", ExtractionResult{
		Extractor: "other", ExtractorVersion: 1, Status: ExtractionOK, Text: "unknown-authority-token",
	})
	seedLegacyMigrationRow(t, s, "obsolete.txt", "64", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 2, Status: ExtractionOK, Text: "obsolete-authority-token",
	})
	invalidHash := fakeHash("65")
	_, err = s.CreateFile(ctx, s.RootID(), "invalid.txt", invalidHash, 1, "text/plain")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO extracted_text(
		blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
	) VALUES(?, 'plain-text', 1, 'ok', NULL, 1, CAST(X'80' AS TEXT), ?)`,
		invalidHash, legacyMigrationTimestamp)
	require.NoError(t, err)
	missingRowHash := fakeHash("66")
	_, err = s.db.Exec(`INSERT INTO extracted_text(
		blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
	) VALUES(?, 'plain-text', 1, 'ok', NULL, 1, 'dangling', ?)`,
		missingRowHash, legacyMigrationTimestamp)
	require.NoError(t, err)
	missingExtractionHash := fakeHash("67")
	_, err = s.CreateFile(ctx, s.RootID(), "missing.txt", missingExtractionHash, 7, "text/plain")
	require.NoError(t, err)

	beforeName, _, err := s.SearchPage(ctx, "report", 20)
	require.NoError(t, err)
	beforeText, _, err := s.SearchPage(ctx, "migration-compatible", 20)
	require.NoError(t, err)
	require.Len(t, beforeName, 1)
	require.Len(t, beforeText, 2)

	report, err := s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, report.EligibleRows)
	assert.Equal(t, 1, report.MigratedBuilds)
	assert.Equal(t, 2, report.MigratedAttachments)
	assert.Equal(t, 4, report.QueuedBlobs)
	require.NotEmpty(t, report.ProfileFingerprint)
	require.NotEmpty(t, report.LexicalGenerationID)

	afterName, _, err := s.SearchPage(ctx, "report", 20)
	require.NoError(t, err)
	afterText, _, err := s.SearchPage(ctx, "migration-compatible", 20)
	require.NoError(t, err)
	assert.Equal(t, beforeName, afterName, "name search remains byte-for-byte compatible")
	assert.Equal(t, beforeText, afterText, "eligible text search keeps exact result order")

	for _, query := range []string{"unknown-authority-token", "obsolete-authority-token"} {
		hits, _, searchErr := s.SearchPage(ctx, query, 20)
		require.NoError(t, searchErr)
		assert.Empty(t, hits, "%s must not keep serving from the portable cache", query)
	}

	var builds, units, segments, attachments, heads, legacyFTS int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_units`).Scan(&units))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_lexical_segments`).Scan(&segments))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&attachments))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
	assert.Equal(t, 1, builds, "a shared blob owns one immutable build")
	assert.Equal(t, 1, units)
	assert.Equal(t, 1, segments)
	assert.Equal(t, 2, attachments)
	assert.Equal(t, 2, heads)
	assert.Zero(t, legacyFTS, "the retained portable cache is non-serving")

	var unitID, evidenceUnitID, storedText string
	require.NoError(t, s.db.QueryRow(`
		SELECT u.unit_id,u.evidence_unit_id,l.text
		FROM rendition_units u JOIN rendition_lexical_segments l USING(build_id)
	`).Scan(&unitID, &evidenceUnitID, &storedText))
	assert.Equal(t, "legacy:0", unitID)
	assert.Equal(t, "legacy:0", evidenceUnitID)
	assert.Equal(t, []byte(exactText), []byte(storedText), "migration must not normalize Unicode or newlines")

	var sourceHash, providerOperation string
	require.NoError(t, s.db.QueryRow(`
		SELECT source_sha256,provider_operation_id FROM rendition_builds
	`).Scan(&sourceHash, &providerOperation))
	assert.Equal(t, eligibleHash, sourceHash)
	assert.Equal(t, "plain-text/legacy-v1", providerOperation)

	var canonicalProfile string
	require.NoError(t, s.db.QueryRow(`SELECT canonical_profile FROM processing_profiles`).Scan(&canonicalProfile))
	assert.Contains(t, canonicalProfile, `"adapter_contract":"plain-text/legacy-v1"`)

	queued := queuedLegacyHashes(t, s)
	assert.Equal(t, []string{
		failedHash, unknownHash, invalidHash, missingExtractionHash,
	}, queued)

	for _, versionID := range []string{first.CurrentVersionID, second.CurrentVersionID} {
		view, activeErr := s.ActiveRendition(ctx, versionID, report.ProfileFingerprint)
		require.NoError(t, activeErr)
		assert.Equal(t, report.ProfileFingerprint, view.Head.ProcessingProfileFingerprint)
		assert.Equal(t, eligibleHash, view.Build.SourceSHA256)
	}

	var retainedRows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&retainedRows))
	assert.Equal(t, 7, retainedRows, "portable legacy rows remain recoverable")
}

// Mutation caught: publishing heads, the lexical generation, cache fencing,
// and queue repair in separate transactions exposes a partial authority epoch.
func TestMigrateLegacyPlainTextRollsBackOneAtomicCutover(t *testing.T) {
	s := newTestStore(t)
	seedLegacyMigrationRow(t, s, "atomic.txt", "71", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "atomic legacy text",
	})
	_, err := s.db.Exec(`
		CREATE TRIGGER reject_legacy_head BEFORE INSERT ON rendition_heads BEGIN
			SELECT RAISE(ABORT, 'synthetic legacy head failure');
		END`)
	require.NoError(t, err)

	_, err = s.MigrateLegacyPlainText(t.Context())
	require.ErrorContains(t, err, "synthetic legacy head failure")

	for _, table := range []string{
		"processing_profiles", "rendition_builds", "rendition_units",
		"rendition_lexical_segments", "rendition_attachments", "rendition_heads",
	} {
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table+" must roll back")
	}
	var legacyFTS int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
	assert.Equal(t, 1, legacyFTS, "legacy serving remains intact when cutover aborts")
}

// Mutation caught: comparing catalog segments instead of the staged serving
// generation publishes even when the exact FTS projection disappears.
func TestMigrateLegacyPlainTextRejectsCorruptStagedProjection(t *testing.T) {
	s := newTestStore(t)
	seedLegacyMigrationRow(t, s, "projection.txt", "77", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "staged-projection-authority",
	})
	_, err := s.db.Exec(lexicalProjectionSchema)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		CREATE TRIGGER erase_staged_projection
		AFTER INSERT ON rendition_lexical_generation_manifests BEGIN
			DELETE FROM rendition_lexical_fts WHERE generation_id=NEW.generation_id;
		END`)
	require.NoError(t, err)

	_, err = s.MigrateLegacyPlainText(t.Context())
	require.ErrorContains(t, err, "search authority is not exactly compatible")
	var legacyFTS, lexicalHeads, renditionHeads int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_lexical_heads`).Scan(&lexicalHeads))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&renditionHeads))
	assert.Equal(t, 1, legacyFTS, "the prior authority survives rejected publication")
	assert.Zero(t, lexicalHeads)
	assert.Zero(t, renditionHeads)
}

// Mutation caught: recording a queued repair only in the fenced legacy cache
// leaves successful fresh work non-serving until the store is reopened.
func TestPostCutoverExtractionPublishesRenditionAuthority(t *testing.T) {
	s := newTestStore(t)
	hash := seedLegacyMigrationRow(t, s, "repair.txt", "72", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionFailed,
		Error: "synthetic pre-cutover failure",
	})
	initial, err := s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "repaired-legacy-authority",
	}))

	var cached, builds, heads int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&cached))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
	assert.Zero(t, cached, "the portable cache remains fenced")
	assert.Equal(t, 1, builds)
	assert.Equal(t, 1, heads)
	var generationID string
	require.NoError(t, s.db.QueryRow(`
		SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1
	`).Scan(&generationID))
	assert.NotEqual(t, initial.LexicalGenerationID, generationID)
	hits, _, err := s.SearchPage(t.Context(), "repaired-legacy-authority", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "/repair.txt", hits[0].Path)
	assert.Equal(t, SearchMatchContent, hits[0].Match)
}

// Mutation caught: rebuilding an existing content-derived identity with the
// latest extraction time rejects its first immutable build and attachment.
func TestPostCutoverIdenticalReExtractionPreservesImmutableRecords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reextract.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		if s != nil {
			require.NoError(t, s.Close())
		}
	})
	const text = "identical-reextraction-authority"
	hash := fakeHash("78")
	first, err := s.CreateFile(
		t.Context(), s.RootID(), "first.txt", hash, int64(len(text)), "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))
	_, err = s.db.Exec(`UPDATE extracted_text SET extracted_at=? WHERE blob_hash=?`,
		legacyMigrationTimestamp, hash)
	require.NoError(t, err)
	_, err = s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)

	var buildID, completedAt, firstAttachmentID, firstAttachedAt string
	require.NoError(t, s.db.QueryRow(`
		SELECT build_id,completed_at FROM rendition_builds
		WHERE source_sha256=?`, hash,
	).Scan(&buildID, &completedAt))
	require.NoError(t, s.db.QueryRow(`
		SELECT attachment_id,attached_at FROM rendition_attachments
		WHERE content_version_id=?`, first.CurrentVersionID,
	).Scan(&firstAttachmentID, &firstAttachedAt))
	assert.Equal(t, legacyMigrationTimestamp, completedAt)
	assert.Equal(t, legacyMigrationTimestamp, firstAttachedAt)

	second, err := s.CreateFile(
		t.Context(), s.RootID(), "second.txt", hash, int64(len(text)), "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))

	var builds, attachments, heads, queued, cached int
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM rendition_builds`:      &builds,
		`SELECT COUNT(*) FROM rendition_attachments`: &attachments,
		`SELECT COUNT(*) FROM rendition_heads`:       &heads,
		`SELECT COUNT(*) FROM text_extraction_queue`: &queued,
		`SELECT COUNT(*) FROM content_fts`:           &cached,
	} {
		require.NoError(t, s.db.QueryRow(query).Scan(destination))
	}
	assert.Equal(t, 1, builds)
	assert.Equal(t, 2, attachments)
	assert.Equal(t, 2, heads)
	assert.Zero(t, queued)
	assert.Zero(t, cached)
	var stableCompletedAt, stableAttachmentID, stableAttachedAt string
	require.NoError(t, s.db.QueryRow(`
		SELECT completed_at FROM rendition_builds WHERE build_id=?`, buildID,
	).Scan(&stableCompletedAt))
	require.NoError(t, s.db.QueryRow(`
		SELECT attachment_id,attached_at FROM rendition_attachments
		WHERE content_version_id=?`, first.CurrentVersionID,
	).Scan(&stableAttachmentID, &stableAttachedAt))
	assert.Equal(t, completedAt, stableCompletedAt)
	assert.Equal(t, firstAttachmentID, stableAttachmentID)
	assert.Equal(t, firstAttachedAt, stableAttachedAt)
	var secondAttachedAt string
	require.NoError(t, s.db.QueryRow(`
		SELECT attached_at FROM rendition_attachments WHERE content_version_id=?`,
		second.CurrentVersionID,
	).Scan(&secondAttachedAt))
	assert.NotEqual(t, legacyMigrationTimestamp, secondAttachedAt)

	hits, _, err := s.SearchPage(t.Context(), text, 20)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.ElementsMatch(t, []string{"/first.txt", "/second.txt"},
		[]string{hits[0].Path, hits[1].Path})
	require.NoError(t, s.Close())
	s = nil
	s, err = Open(dbPath)
	require.NoError(t, err)
	reopened, _, err := s.SearchPage(t.Context(), text, 20)
	require.NoError(t, err)
	require.Len(t, reopened, 2)
}

// Mutation caught: deleting queued work in the cache transaction loses the
// only automatic retry when the later rendition publication rolls back.
func TestPostCutoverPublicationFailureKeepsExtractionQueued(t *testing.T) {
	s := newTestStore(t)
	const text = "retry-publication-authority"
	hash := fakeHash("79")
	_, err := s.CreateFile(
		t.Context(), s.RootID(), "published.txt", hash, int64(len(text)), "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))
	initial, err := s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	_, err = s.CreateFile(
		t.Context(), s.RootID(), "waiting.txt", hash, int64(len(text)), "text/plain",
	)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		CREATE TRIGGER reject_reextraction_head BEFORE INSERT ON rendition_heads BEGIN
			SELECT RAISE(ABORT, 'synthetic reextraction publication failure');
		END`)
	require.NoError(t, err)

	err = s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	})
	require.ErrorContains(t, err, "synthetic reextraction publication failure")
	var queued int
	require.NoError(t, s.db.QueryRow(`
		SELECT COUNT(*) FROM text_extraction_queue WHERE blob_hash=?`, hash,
	).Scan(&queued))
	assert.Equal(t, 1, queued, "failed publication remains retryable")
	var generationID string
	require.NoError(t, s.db.QueryRow(`
		SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1
	`).Scan(&generationID))
	assert.Equal(t, initial.LexicalGenerationID, generationID)
	hits, _, err := s.SearchPage(t.Context(), text, 20)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the prior lexical generation remains sole serving authority")
	assert.Equal(t, "/published.txt", hits[0].Path)

	_, err = s.db.Exec(`DROP TRIGGER reject_reextraction_head`)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))
	require.NoError(t, s.db.QueryRow(`
		SELECT COUNT(*) FROM text_extraction_queue WHERE blob_hash=?`, hash,
	).Scan(&queued))
	assert.Zero(t, queued)
	hits, _, err = s.SearchPage(t.Context(), text, 20)
	require.NoError(t, err)
	require.Len(t, hits, 2)
}

// Mutation caught: indexing one FTS row per bounded rendition segment loses
// legacy whole-document AND matches when terms land in different segments.
func TestLegacyMigrationPreservesSearchAcrossSegmentBoundary(t *testing.T) {
	s := newTestStore(t)
	text := "left-boundary " + strings.Repeat("x", legacyPlainTextSegmentRunes) +
		" right-boundary"
	hash := fakeHash("73")
	_, err := s.CreateFile(
		t.Context(), s.RootID(), "boundary.txt", hash, int64(len(text)), "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))

	before, _, err := s.SearchPage(t.Context(), "left-boundary right-boundary", 20)
	require.NoError(t, err)
	require.Len(t, before, 1, "legacy whole-row FTS proves the compatibility expectation")
	_, err = s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	after, _, err := s.SearchPage(t.Context(), "left-boundary right-boundary", 20)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// Mutation caught: comparing raw legacy catalog segments with their coalesced
// serving row prevents every later rendition head from publishing.
func TestLegacyMigrationAllowsLaterPublicationAfterSegmentCoalescing(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	text := strings.Repeat("x", legacyPlainTextSegmentRunes) + " later-publication-boundary"
	hash := fakeHash("7a")
	_, err := s.CreateFile(ctx, s.RootID(), "legacy-long.txt", hash, int64(len(text)), "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))
	_, err = s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)

	profile := catalogProcessingProfile(t, false)
	build, versionID := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("7b"), "later rendition authority",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("7c"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{
		ID: fakeHash("7d"), VaultID: s.VaultID(), ContentVersionID: versionID,
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T13:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-22T13:01:00.000000000Z",
	}, generation.ID))
}

// Mutation caught: excluding trashed current versions from migrated heads
// leaves their retained text unreachable after restore.
func TestLegacyMigrationRestoresTrashedContentSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	const text = "restored-heritage-authority"
	hash := fakeHash("7e")
	node, err := s.CreateFile(ctx, s.RootID(), "archived.txt", hash, int64(len(text)), "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: text,
	}))
	trashed, _, err := s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)

	_, _, err = s.Restore(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	hits, _, err := s.SearchPage(ctx, text, 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "/archived.txt", hits[0].Path)
	assert.Equal(t, SearchMatchContent, hits[0].Match)
}

// Mutation caught: treating a successful newer extractor result as missing
// work lets the legacy worker replace it with version 1.
func TestLegacyMigrationDoesNotQueueNewerExtractionSuccess(t *testing.T) {
	s := newTestStore(t)
	seedLegacyMigrationRow(t, s, "newer.txt", "7f", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 2,
		Status: ExtractionOK, Text: "newer-extractor-authority",
	})
	_, err := s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)

	pending, err := s.PendingTextExtractions(t.Context(), 20)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// Mutation caught: restricting legacy convergence to released-schema staging
// leaves an already-current database serving only its legacy cache forever.
func TestOpenMigratesLegacyPlainTextInCurrentSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "current.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	seedLegacyMigrationRow(t, s, "current.txt", "74", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "current-schema-legacy-text",
	})
	require.NoError(t, s.Close())

	s, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	var heads, legacyFTS int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
	assert.Equal(t, 1, heads)
	assert.Zero(t, legacyFTS)
	hits, _, err := s.SearchPage(t.Context(), "current-schema-legacy-text", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "/current.txt", hits[0].Path)
}

// Mutation caught: reusing the ordinary enqueue upsert on migration retry
// postpones already-scheduled repair work and makes convergence non-idempotent.
func TestMigrateLegacyPlainTextRetryPreservesQueuedWork(t *testing.T) {
	s := newTestStore(t)
	seedLegacyMigrationRow(t, s, "stable.txt", "76", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "stable immutable legacy build",
	})
	hash := seedLegacyMigrationRow(t, s, "retry.txt", "75", ExtractionResult{
		Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionFailed, Error: "synthetic retry failure",
	})
	first, err := s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	const scheduled = "2030-01-02T03:04:05.000000000Z"
	_, err = s.db.Exec(`UPDATE text_extraction_queue SET next_attempt_at=? WHERE blob_hash=?`, scheduled, hash)
	require.NoError(t, err)

	second, err := s.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first, second)
	var stored string
	require.NoError(t, s.db.QueryRow(
		`SELECT next_attempt_at FROM text_extraction_queue WHERE blob_hash=?`, hash,
	).Scan(&stored))
	assert.Equal(t, scheduled, stored)
	for _, table := range []string{
		"rendition_lexical_generations", "rendition_lexical_generation_manifests",
	} {
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
}

func seedLegacyMigrationRow(
	t *testing.T, s *Store, name, hashSuffix string, result ExtractionResult,
) string {
	t.Helper()
	hash := fakeHash(hashSuffix)
	node, err := s.CreateFile(t.Context(), s.RootID(), name, hash, 32, "text/plain")
	require.NoError(t, err)
	require.NotEmpty(t, node.CurrentVersionID)
	result.BlobHash = hash
	require.NoError(t, s.RecordExtraction(t.Context(), result))
	return hash
}

func queuedLegacyHashes(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT blob_hash FROM text_extraction_queue ORDER BY blob_hash`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var hashes []string
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))
		hashes = append(hashes, hash)
	}
	require.NoError(t, rows.Err())
	sort.Strings(hashes)
	return hashes
}
