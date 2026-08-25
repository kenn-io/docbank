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
}

// MigrateLegacyPlainText retains the portable extracted_text cache while
// moving its exact released plain-text/v1 successes under rendition authority.
// Catalog rows, version heads, the complete lexical projection, legacy FTS
// fencing, and fresh-work repair publish in one SQLite transaction.
func (s *Store) MigrateLegacyPlainText(
	ctx context.Context,
) (LegacyMigrationReport, error) {
	profile, err := legacyPlainTextProfile()
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report := LegacyMigrationReport{ProfileFingerprint: profile.Fingerprint}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		rows, err := readLegacyPlainTextRows(ctx, tx)
		if err != nil {
			return err
		}
		selected, err := readLegacySelectedVersions(ctx, tx)
		if err != nil {
			return err
		}
		if len(rows) == 0 && len(selected) == 0 {
			return nil
		}
		selectedByBlob := make(map[string][]legacySelectedVersion)
		for _, version := range selected {
			selectedByBlob[version.blobHash] = append(selectedByBlob[version.blobHash], version)
		}

		eligible := make(map[string]legacyPlainTextRow)
		for _, row := range rows {
			if row.extractor != legacyPlainTextExtractor ||
				row.extractorVersion != legacyPlainTextExtractorVersion ||
				row.status != ExtractionOK || !row.text.Valid ||
				!utf8.ValidString(row.text.String) {
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

		generation, err := stageLegacyLexicalGenerationTx(ctx, tx)
		if err != nil {
			return err
		}
		report.LexicalGenerationID = generation.ID

		for _, version := range selected {
			row, ok := eligible[version.blobHash]
			if !ok {
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
		}

		if err := compareLegacyServingCompatibilityTx(
			ctx, tx, selected, eligible, profile.Fingerprint, generation.ID,
		); err != nil {
			return err
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
			queued[blobHash] = struct{}{}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO text_extraction_queue(blob_hash,next_attempt_at) VALUES(?,?)
				ON CONFLICT(blob_hash) DO NOTHING`, blobHash, nowRFC3339(),
			); err != nil {
				return fmt.Errorf("queueing legacy plain-text repair for %s: %w", blobHash, err)
			}
		}
		report.QueuedBlobs = len(queued)

		// extracted_text remains portable and recoverable. Its FTS projection is
		// deliberately empty after the new lexical head becomes authoritative.
		if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts`); err != nil {
			return fmt.Errorf("fencing legacy plain-text search authority: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
			ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
			generation.ID,
		); err != nil {
			return fmt.Errorf("publishing migrated legacy lexical head: %w", err)
		}
		return nil
	})
	if err != nil {
		return LegacyMigrationReport{}, fmt.Errorf("migrating legacy plain-text authority: %w", err)
	}
	return report, nil
}

func readLegacyPlainTextRows(
	ctx context.Context, tx *sql.Tx,
) ([]legacyPlainTextRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash,extractor,extractor_version,status,text,extracted_at
		FROM extracted_text ORDER BY blob_hash,extractor`)
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
	ctx context.Context, tx *sql.Tx,
) ([]legacySelectedVersion, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT v.version_id,v.blob_hash,n.name
		FROM text_searchable_versions sv
		JOIN content_versions v ON v.version_id=sv.version_id
		JOIN nodes n ON n.current_version_id=v.version_id
		WHERE n.trashed_at IS NULL
		ORDER BY v.version_id`)
	if err != nil {
		return nil, fmt.Errorf("reading legacy searchable versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []legacySelectedVersion
	for rows.Next() {
		var version legacySelectedVersion
		if err := rows.Scan(&version.versionID, &version.blobHash, &version.name); err != nil {
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
	if _, err := tx.ExecContext(ctx, lexicalProjectionSchema); err != nil {
		return LexicalGeneration{}, fmt.Errorf("initializing migrated lexical projection: %w", err)
	}
	rows, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	generation := LexicalGeneration{
		ID: lexicalManifestDigest(rows), SegmentCount: len(rows),
		ManifestDigest: lexicalManifestDigest(rows),
	}
	var existingCount int
	var existingManifest sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT g.segment_count,m.manifest_digest
		FROM rendition_lexical_generations g
		LEFT JOIN rendition_lexical_generation_manifests m USING(generation_id)
		WHERE g.generation_id=?`, generation.ID,
	).Scan(&existingCount, &existingManifest)
	if err == nil {
		existingRows, err := readLexicalManifestRowsTx(ctx, tx, generation.ID, "")
		if err != nil {
			return LexicalGeneration{}, err
		}
		if !existingManifest.Valid || existingCount != generation.SegmentCount ||
			lexicalManifestDigest(existingRows) != generation.ManifestDigest {
			return LexicalGeneration{}, fmt.Errorf(
				"migrated lexical generation %s has a different immutable manifest", generation.ID,
			)
		}
		return generation, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LexicalGeneration{}, fmt.Errorf("reading migrated lexical generation: %w", err)
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_fts(generation_id,build_id,segment_id,text)
			VALUES(?,?,?,?)`, generation.ID, row.buildID, row.segmentID, row.text,
		); err != nil {
			return LexicalGeneration{}, fmt.Errorf("building migrated lexical generation: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generations(generation_id,segment_count,built_at)
		VALUES(?,?,?)`, generation.ID, generation.SegmentCount, nowRFC3339(),
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("completing migrated lexical generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generation_manifests(generation_id,manifest_digest)
		VALUES(?,?)`, generation.ID, generation.ManifestDigest,
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("recording migrated lexical manifest: %w", err)
	}
	return generation, nil
}

func compareLegacyServingCompatibilityTx(
	ctx context.Context, tx *sql.Tx, selected []legacySelectedVersion,
	eligible map[string]legacyPlainTextRow, profileFingerprint, generationID string,
) error {
	type authority struct{ name, text string }
	want := make(map[string]authority)
	for _, version := range selected {
		if row, ok := eligible[version.blobHash]; ok {
			want[version.versionID] = authority{name: version.name, text: row.text.String}
		}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT h.content_version_id,n.name,f.text
		FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		JOIN rendition_lexical_fts f ON f.build_id=a.build_id
		JOIN content_versions v ON v.version_id=h.content_version_id
		JOIN nodes n ON n.current_version_id=v.version_id
		WHERE h.profile_fingerprint=? AND f.generation_id=? AND n.trashed_at IS NULL
		ORDER BY h.content_version_id,f.segment_id`, profileFingerprint, generationID)
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
