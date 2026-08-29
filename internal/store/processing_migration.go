package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	legacyPlainTextExtractor        = "plain-text"
	legacyPlainTextExtractorVersion = int64(1)
	legacyPlainTextProvider         = "plain-text/legacy-v1"
	legacyPlainTextUnitSourceKey    = "legacy:0"
	legacyPlainTextSegmentRunes     = 1 << 20
)

var legacyPlainTextCapturedPolicy = jsontext.Value(`{"roles":[],"version":1}`)

// LegacyMigrationReport describes one legacy cache-to-catalog cutover. Counts
// name the deterministic desired authority, so a repeated migration reports
// the same result instead of depending on SQLite's insert/no-op distinction.
type LegacyMigrationReport struct {
	EligibleRows        int
	MigratedBuilds      int
	MigratedAttachments int
	QueuedBlobs         int
	ProfileFingerprint  string
	LexicalGenerationID string
}

type legacyPlainTextRow struct {
	blobHash         string
	extractor        string
	extractorVersion int64
	status           string
	text             sql.NullString
	extractedAt      string
}

type legacySelectedVersion struct {
	versionID string
	blobHash  string
	name      string
	trashed   bool
}

// MigrateLegacyPlainText retains the portable extracted_text cache while
// moving its exact released plain-text/v1 successes under rendition authority.
// Catalog rows, version heads, the complete lexical projection, legacy FTS
// fencing, and fresh-work repair publish in one SQLite transaction.
func (s *Store) MigrateLegacyPlainText(
	ctx context.Context,
) (LegacyMigrationReport, error) {
	return s.migrateLegacyPlainText(ctx, "")
}

func (s *Store) migrateLegacyPlainTextBlob(ctx context.Context, blobHash string) error {
	if err := validateCatalogSHA256(blobHash, "legacy plain-text blob hash"); err != nil {
		return err
	}
	_, err := s.migrateLegacyPlainText(ctx, blobHash)
	return err
}

func (s *Store) migrateLegacyPlainText(
	ctx context.Context, blobFilter string,
) (LegacyMigrationReport, error) {
	profile, err := legacyPlainTextProfile()
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report := LegacyMigrationReport{ProfileFingerprint: profile.Fingerprint}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var sourceRevision, migratedRevision int64
		if err := tx.QueryRowContext(ctx, `
			SELECT source_revision,migrated_revision,generation_id,
			       eligible_rows,migrated_builds,migrated_attachments,queued_blobs
			FROM legacy_plain_text_migration_state WHERE singleton=1`,
		).Scan(&sourceRevision, &migratedRevision, &report.LexicalGenerationID,
			&report.EligibleRows, &report.MigratedBuilds,
			&report.MigratedAttachments, &report.QueuedBlobs); err != nil {
			return fmt.Errorf("reading legacy migration state: %w", err)
		}
		if blobFilter == "" && sourceRevision == migratedRevision {
			return nil
		}
		report.EligibleRows = 0
		report.MigratedBuilds = 0
		report.MigratedAttachments = 0
		report.QueuedBlobs = 0
		report.LexicalGenerationID = ""

		rows, err := readLegacyPlainTextRows(ctx, tx, blobFilter)
		if err != nil {
			return err
		}
		selected, err := readLegacySelectedVersions(ctx, tx, blobFilter)
		if err != nil {
			return err
		}
		if blobFilter == "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_plain_text_migration_blobs`); err != nil {
				return fmt.Errorf("resetting legacy plain-text migration report: %w", err)
			}
		}
		selectedByBlob := make(map[string][]legacySelectedVersion)
		for _, version := range selected {
			selectedByBlob[version.blobHash] = append(selectedByBlob[version.blobHash], version)
		}

		eligible := make(map[string]legacyPlainTextRow)
		for _, row := range rows {
			if row.extractor == legacyPlainTextExtractor &&
				row.extractorVersion > legacyPlainTextExtractorVersion &&
				row.status == ExtractionOK && row.text.Valid && utf8.ValidString(row.text.String) {
				if len(selectedByBlob[row.blobHash]) != 0 {
					return fmt.Errorf(
						"unsupported newer plain-text extraction version %d for selected blob %s",
						row.extractorVersion, row.blobHash,
					)
				}
			}
			if row.extractor != legacyPlainTextExtractor ||
				row.extractorVersion != legacyPlainTextExtractorVersion ||
				row.status != ExtractionOK || !row.text.Valid ||
				!utf8.ValidString(row.text.String) {
				continue
			}
			suppressed, err := legacyTextExtractionSuppressedTx(ctx, tx, row.blobHash)
			if err != nil {
				return fmt.Errorf("checking legacy derivative purge suppression: %w", err)
			}
			if suppressed {
				continue
			}
			buildID := legacyPlainTextBuildFingerprint(
				row.blobHash, row.extractor, row.extractorVersion, row.status,
				[]byte(row.text.String))
			buildSuppressed, err := derivativeBuildSuppressedTx(ctx, tx, row.blobHash, buildID)
			if err != nil {
				return fmt.Errorf("checking legacy build purge suppression: %w", err)
			}
			if buildSuppressed {
				continue
			}
			if _, authorityErr := requirePhysicalAuthorityTx(tx, row.blobHash); authorityErr != nil {
				if errors.Is(authorityErr, ErrNotFound) ||
					errors.Is(authorityErr, ErrPhysicalAuthorityMissing) {
					continue
				}
				return fmt.Errorf("checking legacy plain-text blob %s: %w", row.blobHash, authorityErr)
			}
			eligible[row.blobHash] = row
		}
		report.EligibleRows = len(eligible)

		if len(eligible) != 0 {
			if err := ensureProcessingProfileTx(ctx, tx, profile); err != nil {
				return fmt.Errorf("recording legacy plain-text profile: %w", err)
			}
		}
		for _, row := range rows {
			if _, ok := eligible[row.blobHash]; !ok || row.extractor != legacyPlainTextExtractor {
				continue
			}
			build, err := legacyPlainTextBuild(s.vaultID, profile, row)
			if err != nil {
				return err
			}
			if err := insertLegacyRenditionBuildTx(ctx, tx, build); err != nil {
				return err
			}
			report.MigratedBuilds++
		}

		if err := revokeStaleLegacyPlainTextHeadsTx(
			ctx, tx, selected, eligible, profile.Fingerprint, blobFilter,
		); err != nil {
			return err
		}

		attachmentCounts := make(map[string]int)
		for _, version := range selected {
			row, ok := eligible[version.blobHash]
			if !ok {
				continue
			}
			suppressed, err := legacyDerivativeSuppressedTx(
				ctx, tx, version.blobHash, version.versionID, profile.Fingerprint)
			if err != nil {
				return fmt.Errorf("checking legacy attachment purge suppression: %w", err)
			}
			if suppressed {
				continue
			}
			buildID := legacyPlainTextBuildFingerprint(
				row.blobHash, row.extractor, row.extractorVersion, row.status,
				[]byte(row.text.String),
			)
			attachmentID := legacyPlainTextAttachmentID(version.versionID, buildID)
			attachment := RenditionAttachmentRecord{
				ID: attachmentID, VaultID: s.vaultID, ContentVersionID: version.versionID,
				BuildID: buildID, Profile: profile, AttachedAt: row.extractedAt,
			}
			if err := insertLegacyRenditionAttachmentTx(ctx, tx, attachment); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rendition_heads(
					content_version_id,profile_fingerprint,attachment_id,published_at
				) VALUES(?,?,?,?)
				ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET
					attachment_id=excluded.attachment_id,published_at=excluded.published_at`,
				version.versionID, profile.Fingerprint, attachmentID, row.extractedAt,
			); err != nil {
				return fmt.Errorf("publishing migrated legacy plain-text head: %w", err)
			}
			report.MigratedAttachments++
			attachmentCounts[version.blobHash]++
		}

		generation := LexicalGeneration{}
		if len(rows) != 0 || len(selected) != 0 {
			generation, err = stageLegacyLexicalGenerationTx(ctx, tx)
			if err != nil {
				return err
			}
			report.LexicalGenerationID = generation.ID
		}

		if generation.ID != "" {
			if err := compareLegacyServingCompatibilityTx(
				ctx, tx, selected, eligible, profile.Fingerprint, generation.ID, blobFilter,
			); err != nil {
				return err
			}
		}

		queued := make(map[string]struct{})
		for blobHash := range selectedByBlob {
			if _, ok := eligible[blobHash]; ok {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM text_extraction_queue WHERE blob_hash=?`, blobHash,
				); err != nil {
					return fmt.Errorf("clearing migrated legacy plain-text work: %w", err)
				}
				continue
			}
			suppressed, err := legacyTextExtractionSuppressedTx(ctx, tx, blobHash)
			if err != nil {
				return fmt.Errorf("checking legacy repair purge suppression: %w", err)
			}
			if suppressed {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM text_extraction_queue WHERE blob_hash=?`, blobHash); err != nil {
					return fmt.Errorf("clearing suppressed legacy repair work: %w", err)
				}
				continue
			}
			queued[blobHash] = struct{}{}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO text_extraction_queue(blob_hash,next_attempt_at) VALUES(?,?)
				ON CONFLICT(blob_hash) DO NOTHING`, blobHash, nowRFC3339(),
			); err != nil {
				return fmt.Errorf("queueing legacy plain-text repair for %s: %w", blobHash, err)
			}
		}
		report.QueuedBlobs = len(queued)
		stateBlobs := make(map[string]struct{})
		for _, row := range rows {
			stateBlobs[row.blobHash] = struct{}{}
		}
		for blobHash := range selectedByBlob {
			stateBlobs[blobHash] = struct{}{}
		}
		for blobHash := range stateBlobs {
			_, isEligible := eligible[blobHash]
			_, isQueued := queued[blobHash]
			if _, err := tx.ExecContext(ctx, `INSERT INTO legacy_plain_text_migration_blobs(
				blob_hash,eligible,attachment_count,queued
			) VALUES(?,?,?,?) ON CONFLICT(blob_hash) DO UPDATE SET
				eligible=excluded.eligible,
				attachment_count=excluded.attachment_count,
				queued=excluded.queued`, blobHash, isEligible, attachmentCounts[blobHash], isQueued,
			); err != nil {
				return fmt.Errorf("recording legacy plain-text blob migration state: %w", err)
			}
		}

		// extracted_text remains portable and recoverable. Its FTS projection is
		// deliberately empty after the new lexical head becomes authoritative.
		if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts`); err != nil {
			return fmt.Errorf("fencing legacy plain-text search authority: %w", err)
		}
		if generation.ID != "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
				ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
				generation.ID,
			); err != nil {
				return fmt.Errorf("publishing migrated legacy lexical head: %w", err)
			}
			if err := s.collectSupersededLegacyLexicalGenerationsTx(
				ctx, tx, generation.ID, nowRFC3339(),
			); err != nil {
				return err
			}
		}
		if blobFilter == "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_plain_text_migration_dirty_blobs`); err != nil {
				return fmt.Errorf("clearing completed legacy plain-text migration sources: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx,
			`DELETE FROM legacy_plain_text_migration_dirty_blobs WHERE blob_hash=?`, blobFilter,
		); err != nil {
			return fmt.Errorf("clearing completed legacy plain-text blob source: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(eligible),0),COALESCE(SUM(eligible),0),
			COALESCE(SUM(attachment_count),0),COALESCE(SUM(queued),0)
			FROM legacy_plain_text_migration_blobs`).Scan(
			&report.EligibleRows, &report.MigratedBuilds,
			&report.MigratedAttachments, &report.QueuedBlobs,
		); err != nil {
			return fmt.Errorf("reading completed legacy plain-text migration report: %w", err)
		}
		var dirty bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM legacy_plain_text_migration_dirty_blobs
		)`).Scan(&dirty); err != nil {
			return fmt.Errorf("reading dirty legacy plain-text migration sources: %w", err)
		}
		return storeLegacyMigrationStateTx(ctx, tx, sourceRevision, report, !dirty)
	})
	if err != nil {
		return LegacyMigrationReport{}, fmt.Errorf("migrating legacy plain-text authority: %w", err)
	}
	return report, nil
}

func storeLegacyMigrationStateTx(
	ctx context.Context, tx *sql.Tx, sourceRevision int64, report LegacyMigrationReport,
	completed bool,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE legacy_plain_text_migration_state SET
			migrated_revision=CASE WHEN ? THEN ? ELSE migrated_revision END,
			generation_id=?,eligible_rows=?,migrated_builds=?,
			migrated_attachments=?,queued_blobs=?
		WHERE singleton=1 AND source_revision=?`,
		completed, sourceRevision, report.LexicalGenerationID, report.EligibleRows,
		report.MigratedBuilds, report.MigratedAttachments, report.QueuedBlobs,
		sourceRevision,
	); err != nil {
		return fmt.Errorf("recording legacy migration state: %w", err)
	}
	return nil
}

func (s *Store) collectSupersededLegacyLexicalGenerationsTx(
	ctx context.Context, tx *sql.Tx, activeGenerationID, asOf string,
) error {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	generationIDs, err := func() (_ []string, retErr error) {
		rows, err := tx.QueryContext(ctx, `
			SELECT g.generation_id
			FROM rendition_lexical_generations g
			WHERE g.generation_id<>?
			  AND NOT EXISTS(
				SELECT 1 FROM rendition_lexical_heads h
				WHERE h.generation_id=g.generation_id
			  )
			  AND NOT EXISTS(
				SELECT 1 FROM current_rendition_roots r
				WHERE r.target_kind='lexical_generation'
				  AND r.target_id=g.generation_id
				  AND r.active=1
				  AND (r.expires_at IS NULL OR r.expires_at>?)
			  )
			ORDER BY g.generation_id`, activeGenerationID, asOf)
		if err != nil {
			return nil, fmt.Errorf("listing superseded legacy lexical generations: %w", err)
		}
		defer func() {
			retErr = errors.Join(retErr, rows.Close())
		}()
		var result []string
		for rows.Next() {
			var generationID string
			if err := rows.Scan(&generationID); err != nil {
				return nil, fmt.Errorf("scanning superseded legacy lexical generation: %w", err)
			}
			if lexicalGenerationReaders.stores[s][generationID] == 0 {
				result = append(result, generationID)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("listing superseded legacy lexical generations: %w", err)
		}
		return result, nil
	}()
	if err != nil {
		return err
	}
	for _, generationID := range generationIDs {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rendition_lexical_fts WHERE generation_id=?`, generationID); err != nil {
			return fmt.Errorf("removing superseded lexical rows %s: %w", generationID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rendition_lexical_generation_manifests WHERE generation_id=?`,
			generationID); err != nil {
			return fmt.Errorf("removing superseded lexical manifest %s: %w", generationID, err)
		}
		if err := releaseCollectedLexicalGenerationTx(ctx, tx, generationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rendition_lexical_generations WHERE generation_id=?`,
			generationID); err != nil {
			return fmt.Errorf("removing superseded lexical generation %s: %w", generationID, err)
		}
	}
	return nil
}

func releaseCollectedLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID string,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs
		SET lexical_generation_id=NULL
		WHERE phase='published' AND lexical_generation_id=?`, generationID); err != nil {
		return fmt.Errorf("releasing collected lexical generation %s from completed jobs: %w",
			generationID, err)
	}
	return nil
}

func revokeStaleLegacyPlainTextHeadsTx(
	ctx context.Context, tx *sql.Tx, selected []legacySelectedVersion,
	eligible map[string]legacyPlainTextRow, profileFingerprint, blobFilter string,
) error {
	expected := make(map[string]string, len(selected))
	for _, version := range selected {
		row, ok := eligible[version.blobHash]
		if !ok {
			continue
		}
		suppressed, err := legacyDerivativeSuppressedTx(
			ctx, tx, version.blobHash, version.versionID, profileFingerprint)
		if err != nil {
			return fmt.Errorf("checking legacy head purge suppression: %w", err)
		}
		if suppressed {
			continue
		}
		buildID := legacyPlainTextBuildFingerprint(
			row.blobHash, row.extractor, row.extractorVersion, row.status, []byte(row.text.String),
		)
		expected[version.versionID] = legacyPlainTextAttachmentID(version.versionID, buildID)
	}
	type staleHead struct{ versionID, attachmentID string }
	stale, err := func() (_ []staleHead, retErr error) {
		query := `
			SELECT h.content_version_id,h.attachment_id
			FROM rendition_heads h
			JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
			JOIN rendition_builds b ON b.build_id=a.build_id
			JOIN content_versions v ON v.version_id=h.content_version_id
			WHERE h.profile_fingerprint=? AND b.provider_operation_id=?
			ORDER BY h.content_version_id`
		args := []any{profileFingerprint, legacyPlainTextProvider}
		if blobFilter != "" {
			query = strings.Replace(query, "\n\t\t\tORDER BY", " AND v.blob_hash=?\n\t\t\tORDER BY", 1)
			args = append(args, blobFilter)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("reading legacy rendition heads for replacement: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		var result []staleHead
		for rows.Next() {
			var head staleHead
			if err := rows.Scan(&head.versionID, &head.attachmentID); err != nil {
				return nil, fmt.Errorf("scanning legacy rendition head for replacement: %w", err)
			}
			if expected[head.versionID] != head.attachmentID {
				result = append(result, head)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("reading legacy rendition heads for replacement: %w", err)
		}
		return result, nil
	}()
	if err != nil {
		return err
	}
	for _, head := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_heads
			WHERE content_version_id=? AND profile_fingerprint=? AND attachment_id=?`,
			head.versionID, profileFingerprint, head.attachmentID); err != nil {
			return fmt.Errorf("revoking stale legacy rendition head %s: %w", head.attachmentID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_attachments
			WHERE attachment_id=?`, head.attachmentID); err != nil {
			return fmt.Errorf("removing stale legacy rendition attachment %s: %w", head.attachmentID, err)
		}
	}
	return nil
}

func readLegacyPlainTextRows(
	ctx context.Context, tx *sql.Tx, blobFilter string,
) ([]legacyPlainTextRow, error) {
	query := `
		SELECT blob_hash,extractor,extractor_version,status,text,extracted_at
		FROM extracted_text`
	args := []any(nil)
	if blobFilter != "" {
		query += ` WHERE blob_hash=?`
		args = append(args, blobFilter)
	}
	query += ` ORDER BY blob_hash,extractor`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading legacy extracted-text cache: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []legacyPlainTextRow
	for rows.Next() {
		var row legacyPlainTextRow
		if err := rows.Scan(
			&row.blobHash, &row.extractor, &row.extractorVersion,
			&row.status, &row.text, &row.extractedAt,
		); err != nil {
			return nil, fmt.Errorf("reading legacy extracted-text row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading legacy extracted-text cache: %w", err)
	}
	return result, nil
}

func readLegacySelectedVersions(
	ctx context.Context, tx *sql.Tx, blobFilter string,
) ([]legacySelectedVersion, error) {
	query := `
		SELECT v.version_id,v.blob_hash,n.name,n.trashed_at IS NOT NULL
		FROM text_searchable_versions sv
		JOIN content_versions v ON v.version_id=sv.version_id
		JOIN nodes n ON n.current_version_id=v.version_id`
	args := []any(nil)
	if blobFilter != "" {
		query += ` WHERE v.blob_hash=?`
		args = append(args, blobFilter)
	}
	query += ` ORDER BY v.version_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading legacy searchable versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []legacySelectedVersion
	for rows.Next() {
		var version legacySelectedVersion
		if err := rows.Scan(
			&version.versionID, &version.blobHash, &version.name, &version.trashed,
		); err != nil {
			return nil, fmt.Errorf("reading legacy searchable version: %w", err)
		}
		result = append(result, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading legacy searchable versions: %w", err)
	}
	return result, nil
}

func legacyPlainTextProfile() (ProcessingProfileRecord, error) {
	fingerprint := func(field string) string {
		digest := sha256.Sum256([]byte("docbank-legacy-plain-text-profile/v1\x00" + field))
		return hex.EncodeToString(digest[:])
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{ //nolint:gosec // Contains a non-secret credential:<name> reference.
			AdapterContract:          legacyPlainTextProvider,
			AuthorizationFingerprint: fingerprint("authorization"),
			CredentialBinding:        "credential:legacy-import",
			DeploymentFingerprint:    fingerprint("deployment"),
			Descriptor: document.ProviderDescriptorV1{
				ID: legacyPlainTextProvider, Fingerprint: fingerprint("descriptor"),
			},
			DisclosureFingerprint: fingerprint("disclosure"), MaxDocumentBytes: 1 << 40,
			MaxResponseBytes: 1 << 30, MaxUnits: 1, Name: "plain-text-legacy-v1",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "local-vault", UploadOptionsFingerprint: fingerprint("upload-options"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     fingerprint("degraded-provenance"),
			LexicalSegmenterFingerprint: fingerprint("exact-stored-text"),
			MaxSegmentRunes:             legacyPlainTextSegmentRunes, MaxUnitRunes: 256 << 20,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      fingerprint("no-normalization"),
			RenditionContract:          document.RenditionContractV1,
			SanitizerFingerprint:       fingerprint("legacy-trusted-local-cache"),
			SourceEvidenceContract:     document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: fingerprint("attachment-policy"),
			ConsentFingerprint:          fingerprint("legacy-local-consent"),
			RetainTypedArtifacts:        true, TrustBoundary: "local-vault",
		},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil {
		return ProcessingProfileRecord{}, fmt.Errorf("constructing legacy plain-text profile: %w", err)
	}
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}, nil
}

func legacyPlainTextBuild(
	vaultID string, profile ProcessingProfileRecord, row legacyPlainTextRow,
) (RenditionBuildRecord, error) {
	textBytes := []byte(row.text.String)
	textDigest := sha256.Sum256(textBytes)
	textChecksum := hex.EncodeToString(textDigest[:])
	buildID := legacyPlainTextBuildFingerprint(
		row.blobHash, row.extractor, row.extractorVersion, row.status, textBytes,
	)
	segments := legacyPlainTextSegments(row.text.String)
	policyFingerprint := sha256.Sum256(legacyPlainTextCapturedPolicy)
	authorization := sha256.Sum256([]byte("docbank-legacy-plain-text-authorization/v1"))
	return normalizeRenditionBuildRecord(RenditionBuildRecord{
		ID: buildID, VaultID: vaultID, SourceSHA256: row.blobHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: hex.EncodeToString(policyFingerprint[:]),
		CapturedArtifactPolicy:            append(jsontext.Value(nil), legacyPlainTextCapturedPolicy...),
		AuthorizationChecksum:             hex.EncodeToString(authorization[:]),
		ProviderOperationID:               legacyPlainTextProvider,
		ProviderReceipt: jsontext.Value(
			`{"extractor":"plain-text","extractor_version":1,"migration":"legacy-v1","status":"ok"}`,
		),
		EvidenceChecksum: textChecksum, RenditionChecksum: buildID, MarkdownChecksum: textChecksum,
		Completeness: document.EvidenceDegradedProvenance, Warnings: []string{},
		CompletedAt: row.extractedAt, DeclaredArtifactCount: 0,
		Units: []RenditionUnitRecord{{
			ID: legacyPlainTextUnitSourceKey, EvidenceUnitID: legacyPlainTextUnitSourceKey,
			Order: 0, Checksum: textChecksum, HeadingPath: []string{},
			Locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
		LexicalSegments: segments,
	})
}

func legacyPlainTextSegments(text string) []RenditionLexicalSegmentRecord {
	var result []RenditionLexicalSegmentRecord
	byteStart, runeStart := 0, 0
	for byteStart < len(text) || byteStart == 0 && len(text) == 0 {
		byteEnd := byteStart
		runes := 0
		for byteEnd < len(text) && runes < legacyPlainTextSegmentRunes {
			_, size := utf8.DecodeRuneInString(text[byteEnd:])
			byteEnd += size
			runes++
		}
		segmentText := text[byteStart:byteEnd]
		digest := sha256.Sum256([]byte(segmentText))
		index := len(result)
		result = append(result, RenditionLexicalSegmentRecord{
			ID: "legacy:" + strconv.Itoa(index), UnitID: legacyPlainTextUnitSourceKey,
			Order: index, CharStart: runeStart, CharEnd: runeStart + runes,
			Checksum: hex.EncodeToString(digest[:]), Text: segmentText,
		})
		byteStart = byteEnd
		runeStart += runes
		if byteStart == len(text) {
			break
		}
	}
	return result
}

func legacyPlainTextBuildFingerprint(
	blobHash, extractor string, version int64, status string, text []byte,
) string {
	textChecksum := sha256.Sum256(text)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "docbank-legacy-plain-text/v1\x00")
	for _, field := range []string{blobHash, extractor, strconv.FormatInt(version, 10), status} {
		_, _ = io.WriteString(hash, field)
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(textChecksum[:])
	return hex.EncodeToString(hash.Sum(nil))
}

func legacyPlainTextAttachmentID(versionID, buildID string) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, "docbank-legacy-plain-text-attachment/v1\x00")
	_, _ = io.WriteString(hash, versionID)
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, buildID)
	return hex.EncodeToString(hash.Sum(nil))
}

func insertLegacyRenditionBuildTx(
	ctx context.Context, tx *sql.Tx, record RenditionBuildRecord,
) error {
	suppressed, err := derivativeBuildSuppressedTx(ctx, tx, record.SourceSHA256, record.ID)
	if err != nil {
		return fmt.Errorf("checking legacy rendition build purge suppression: %w", err)
	}
	if suppressed {
		return fmt.Errorf("legacy rendition build %s has an active purge suppression", record.ID)
	}
	if err := validateRenditionBuildBlobAuthorityTx(ctx, tx, record); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO rendition_builds(
			build_id,vault_uid,source_sha256,rendition_request_fingerprint,
			evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
			captured_artifact_policy_json,authorization_checksum,provider_operation_id,
			provider_receipt_json,evidence_checksum,rendition_checksum,markdown_checksum,
			completeness,partial_success,truncated,warnings_json,completed_at,
			declared_artifact_count,unit_count,lexical_segment_count
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.ID, record.VaultID, record.SourceSHA256,
		record.RenditionRequestFingerprint, record.EvidenceLexicalFingerprint,
		record.CapturedArtifactPolicyFingerprint, string(record.CapturedArtifactPolicy),
		record.AuthorizationChecksum, record.ProviderOperationID, string(record.ProviderReceipt),
		record.EvidenceChecksum, record.RenditionChecksum, record.MarkdownChecksum,
		record.Completeness, record.PartialSuccess, record.Truncated,
		mustCatalogJSON(record.Warnings), record.CompletedAt, record.DeclaredArtifactCount,
		len(record.Units), len(record.LexicalSegments),
	)
	if err != nil {
		return fmt.Errorf("inserting legacy rendition build %s: %w", record.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking legacy rendition build %s: %w", record.ID, err)
	}
	if inserted != 0 {
		if err := insertRenditionBuildChildrenTx(ctx, tx, record); err != nil {
			return err
		}
	} else {
		stored, err := loadRenditionBuild(ctx, tx, record.ID)
		if err != nil {
			return err
		}
		record.CompletedAt = stored.CompletedAt
		if !legacyPlainTextBuildEqual(stored, record) {
			return fmt.Errorf("legacy rendition build %s names different immutable metadata", record.ID)
		}
	}
	return validateRenditionBuildStateTx(ctx, tx, record.ID)
}

func legacyPlainTextBuildEqual(left, right RenditionBuildRecord) bool {
	canonicalizeEmptyLists := func(record RenditionBuildRecord) RenditionBuildRecord {
		if len(record.Warnings) == 0 {
			record.Warnings = nil
		}
		if len(record.Artifacts) == 0 {
			record.Artifacts = nil
		}
		record.Units = append([]RenditionUnitRecord(nil), record.Units...)
		for i := range record.Units {
			if len(record.Units[i].HeadingPath) == 0 {
				record.Units[i].HeadingPath = nil
			}
		}
		return record
	}
	return reflect.DeepEqual(canonicalizeEmptyLists(left), canonicalizeEmptyLists(right))
}

func insertLegacyRenditionAttachmentTx(
	ctx context.Context, tx *sql.Tx, record RenditionAttachmentRecord,
) error {
	normalized, err := normalizeRenditionAttachmentRecord(record)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO rendition_attachments(
			attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
			retention_disclosure_fingerprint,attachment_policy_fingerprint,
			consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		normalized.ID, normalized.VaultID, normalized.ContentVersionID, normalized.BuildID,
		normalized.Profile.Fingerprint, normalized.Profile.RetentionDisclosureFingerprint,
		normalized.Profile.AttachmentPolicyFingerprint, normalized.Profile.ConsentFingerprint,
		normalized.Profile.RenditionDisclosureFingerprint, normalized.Profile.TrustBoundary,
		normalized.AttachedAt,
	)
	if err != nil {
		return fmt.Errorf("inserting legacy rendition attachment %s: %w", normalized.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		stored, err := loadRenditionAttachment(ctx, tx, normalized.ID)
		if err != nil {
			return err
		}
		normalized.AttachedAt = stored.AttachedAt
		if !reflect.DeepEqual(stored, normalized) {
			return fmt.Errorf("legacy rendition attachment %s names different immutable metadata", normalized.ID)
		}
	}
	return nil
}

func stageLegacyLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx,
) (LexicalGeneration, error) {
	rows, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	return stageLexicalGenerationTx(ctx, tx, lexicalReplacementGenerationID(rows, buildIDs))
}

func compareLegacyServingCompatibilityTx(
	ctx context.Context, tx *sql.Tx, selected []legacySelectedVersion,
	eligible map[string]legacyPlainTextRow, profileFingerprint, generationID, blobFilter string,
) error {
	type authority struct{ name, text string }
	want := make(map[string]authority)
	for _, version := range selected {
		if version.trashed {
			continue
		}
		row, ok := eligible[version.blobHash]
		if !ok {
			continue
		}
		suppressed, err := legacyDerivativeSuppressedTx(
			ctx, tx, version.blobHash, version.versionID, profileFingerprint)
		if err != nil {
			return fmt.Errorf("checking legacy compatibility purge suppression: %w", err)
		}
		if suppressed {
			continue
		}
		want[version.versionID] = authority{name: version.name, text: row.text.String}
	}
	query := `
		SELECT h.content_version_id,n.name,f.text
		FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		JOIN rendition_lexical_fts f ON f.build_id=a.build_id
		JOIN content_versions v ON v.version_id=h.content_version_id
		JOIN nodes n ON n.current_version_id=v.version_id
		WHERE h.profile_fingerprint=? AND f.generation_id=? AND n.trashed_at IS NULL`
	args := []any{profileFingerprint, generationID}
	if blobFilter != "" {
		query += ` AND v.blob_hash=?`
		args = append(args, blobFilter)
	}
	query += ` ORDER BY h.content_version_id,f.segment_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("comparing migrated search compatibility: %w", err)
	}
	defer func() { _ = rows.Close() }()
	got := make(map[string]authority)
	for rows.Next() {
		var versionID, name, text string
		if err := rows.Scan(&versionID, &name, &text); err != nil {
			return fmt.Errorf("comparing migrated search row: %w", err)
		}
		item := got[versionID]
		if item.name == "" {
			item.name = name
		} else if item.name != name {
			return errors.New("migrated name search authority changed during cutover")
		}
		item.text += text
		got[versionID] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("comparing migrated search compatibility: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("migrated name/text search authority is not exactly compatible")
	}
	return nil
}
