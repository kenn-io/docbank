package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
)

// BlobInfo identifies a recorded blob.
type BlobInfo struct {
	Hash string
	Size int64
}

// GCLooseRetirement is durable retry authority for one loose physical object
// whose logical blob membership has already been removed.
type GCLooseRetirement struct {
	StoreID  string
	Hash     packstore.Hash
	Encoding packstore.LooseEncoding
}

// GCPackRetirement preserves exact physical-erasure authority after the blob
// rows owning dead packed entries have been removed.
type GCPackRetirement struct {
	StoreID string
	PackID  string
}

// DerivativeBuildGC identifies one complete immutable rendition manifest that
// has no current root. ArtifactBlobHashes are the physical derivative objects
// that become eligible for ordinary blob GC when this build is removed.
type DerivativeBuildGC struct {
	BuildID            string
	ArtifactBlobHashes []string
}

// CurrentRenditionRootKind identifies the producer retaining an exact current
// rendition build or lexical generation.
type CurrentRenditionRootKind string

const (
	RenditionRootAttachment  CurrentRenditionRootKind = "attachment"
	RenditionRootHead        CurrentRenditionRootKind = "head"
	RenditionRootRetention   CurrentRenditionRootKind = "retention"
	RenditionRootAudit       CurrentRenditionRootKind = "audit"
	RenditionRootJob         CurrentRenditionRootKind = "job"
	RenditionRootReaderLease CurrentRenditionRootKind = "reader_lease"
	RenditionRootWorkerLease CurrentRenditionRootKind = "worker_lease"
	RenditionRootBackupPin   CurrentRenditionRootKind = "backup_pin"
)

// CurrentRenditionTargetKind distinguishes immutable catalog builds from
// rebuildable lexical projection generations.
type CurrentRenditionTargetKind string

const (
	RenditionRootBuild             CurrentRenditionTargetKind = "rendition_build"
	RenditionRootLexicalGeneration CurrentRenditionTargetKind = "lexical_generation"
)

// CurrentRenditionRoot is one fenced exact-root grant. Reader and worker
// leases require ExpiresAt; every other root remains until explicit release.
type CurrentRenditionRoot struct {
	ID           string
	Kind         CurrentRenditionRootKind
	TargetKind   CurrentRenditionTargetKind
	TargetID     string
	FencingToken int64
	RecordedAt   string
	ExpiresAt    string
}

// ErrCurrentRenditionRootFenced reports a stale create, renewal, or release.
var ErrCurrentRenditionRootFenced = errors.New("current rendition root is fenced")

// DerivativeGCPlan is a deterministic inventory of unreachable rendition
// builds and lexical generations. Planning never mutates live authority.
type DerivativeGCPlan struct {
	Builds             []DerivativeBuildGC
	LexicalGenerations []string
	ExpiredRootIDs     []string
}

// PurgeRequest selects live rendition attachments or exact builds. All is an
// explicit vault-wide derivative purge; an empty request performs only
// ordinary unreachable derivative collection.
type PurgeRequest struct {
	ContentVersionIDs []string
	AttachmentIDs     []string
	BuildIDs          []string
	All               bool
}

// PurgeReport is the complete live-vault mutation receipt. Physical derivative
// blobs remain cataloged for the ordinary location-aware GC pass named by
// PhysicalDerivativeBlobsPendingGC. Immutable backup repositories are outside
// this mutation boundary and are never rewritten.
type PurgeReport struct {
	RemovedHeads                     int
	RemovedAttachments               int
	RemovedBuilds                    int
	RemovedArtifacts                 int
	RemovedUnits                     int
	RemovedLexicalSegments           int
	RemovedLexicalGenerations        int
	RemovedLexicalRows               int
	RemovedLegacyCacheRows           int
	ExpiredRootsRemoved              int
	RetainedBuildIDs                 []string
	RetainedLexicalGenerations       []string
	PhysicalDerivativeBlobsPendingGC []string
	ImmutableBackupCopiesUntouched   bool
}

type purgeAttachment struct {
	id, contentVersionID, buildID, profileFingerprint string
}

func renditionAttachmentsForPurgeTx(
	ctx context.Context, tx *sql.Tx,
) (_ []purgeAttachment, retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT attachment_id,content_version_id,build_id,profile_fingerprint
		FROM rendition_attachments ORDER BY attachment_id`)
	if err != nil {
		return nil, fmt.Errorf("selecting rendition attachments for purge: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing rendition attachment selection: %w", err))
		}
	}()
	var attachments []purgeAttachment
	for rows.Next() {
		var attachment purgeAttachment
		if err := rows.Scan(&attachment.id, &attachment.contentVersionID,
			&attachment.buildID, &attachment.profileFingerprint); err != nil {
			return nil, fmt.Errorf("scanning rendition attachment for purge: %w", err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("selecting rendition attachments for purge: %w", err)
	}
	return attachments, nil
}

func renditionBuildIDsForPurgeTx(
	ctx context.Context, tx *sql.Tx,
) ([]string, error) {
	return stringColumnTx(ctx, tx, "rendition builds for purge",
		`SELECT build_id FROM rendition_builds ORDER BY build_id`)
}

func stringColumnTx(
	ctx context.Context, tx *sql.Tx, subject, query string, args ...any,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", subject, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing %s: %w", subject, err))
		}
	}()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scanning %s: %w", subject, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying %s: %w", subject, err)
	}
	return values, nil
}

// PurgeDerivatives revokes selected live attachment authority and atomically
// collects every complete build/generation manifest no exact root still needs.
func (s *Store) PurgeDerivatives(
	ctx context.Context, request PurgeRequest,
) (PurgeReport, error) {
	if err := validatePurgeRequest(request); err != nil {
		return PurgeReport{}, err
	}
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	report := PurgeReport{ImmutableBackupCopiesUntouched: true}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		auditActive, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		asOf := nowRFC3339()
		result, err := tx.ExecContext(ctx, `
			UPDATE current_rendition_roots SET active=0,released_at=?
			WHERE active=1 AND expires_at IS NOT NULL AND expires_at<=?`, asOf, asOf)
		if err != nil {
			return fmt.Errorf("removing expired current rendition roots: %w", err)
		}
		if report.ExpiredRootsRemoved, err = rowsAffectedInt(result); err != nil {
			return fmt.Errorf("counting expired current rendition roots: %w", err)
		}

		versionSet := stringSet(request.ContentVersionIDs)
		attachmentSet := stringSet(request.AttachmentIDs)
		explicitBuilds := stringSet(request.BuildIDs)
		requestedBuilds := stringSet(request.BuildIDs)
		attachments, err := renditionAttachmentsForPurgeTx(ctx, tx)
		if err != nil {
			return err
		}
		var selected []purgeAttachment
		for _, attachment := range attachments {
			_, versionSelected := versionSet[attachment.contentVersionID]
			_, attachmentSelected := attachmentSet[attachment.id]
			_, buildSelected := explicitBuilds[attachment.buildID]
			if request.All || versionSelected || attachmentSelected || buildSelected {
				selected = append(selected, attachment)
				requestedBuilds[attachment.buildID] = struct{}{}
			}
		}
		if request.All {
			buildIDs, err := renditionBuildIDsForPurgeTx(ctx, tx)
			if err != nil {
				return err
			}
			for _, buildID := range buildIDs {
				requestedBuilds[buildID] = struct{}{}
			}
		}
		for _, attachment := range selected {
			result, err := tx.ExecContext(ctx,
				`DELETE FROM rendition_heads WHERE attachment_id=?`, attachment.id)
			if err != nil {
				return fmt.Errorf("removing rendition head for attachment %s: %w", attachment.id, err)
			}
			count, err := rowsAffectedInt(result)
			if err != nil {
				return err
			}
			report.RemovedHeads += count
			result, err = tx.ExecContext(ctx,
				`DELETE FROM rendition_attachments WHERE attachment_id=?`, attachment.id)
			if err != nil {
				return fmt.Errorf("removing rendition attachment %s: %w", attachment.id, err)
			}
			count, err = rowsAffectedInt(result)
			if err != nil {
				return err
			}
			report.RemovedAttachments += count
		}

		candidateBuilds := make(map[string]struct{})
		allBuildIDs, err := renditionBuildIDsForPurgeTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, buildID := range allBuildIDs {
			var rooted bool
			if err := tx.QueryRowContext(ctx, `SELECT
				EXISTS(SELECT 1 FROM rendition_attachments WHERE build_id=?) OR
				EXISTS(SELECT 1 FROM current_rendition_roots
				       WHERE target_kind='rendition_build' AND target_id=?
				         AND active=1
				         AND (expires_at IS NULL OR expires_at>?))`, buildID, buildID, asOf,
			).Scan(&rooted); err != nil {
				return fmt.Errorf("checking rendition build %s roots: %w", buildID, err)
			}
			if rooted {
				if _, selected := requestedBuilds[buildID]; selected {
					report.RetainedBuildIDs = append(report.RetainedBuildIDs, buildID)
				}
				continue
			}
			candidateBuilds[buildID] = struct{}{}
		}
		logicallyPurgedBuilds := make(map[string]struct{})
		for buildID := range requestedBuilds {
			var exists, attached bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM rendition_builds WHERE build_id=?),
				        EXISTS(SELECT 1 FROM rendition_attachments WHERE build_id=?)`, buildID, buildID,
			).Scan(&exists, &attached); err != nil {
				return fmt.Errorf("checking rendition build %s remaining attachments: %w", buildID, err)
			}
			if exists && !attached {
				logicallyPurgedBuilds[buildID] = struct{}{}
			}
		}
		suppressionChanges, err := installDerivativePurgeSuppressionsTx(
			ctx, tx, logicallyPurgedBuilds, selected, asOf)
		if err != nil {
			return err
		}
		legacyVersionSet := make(map[string]struct{}, len(versionSet)+len(selected))
		for versionID := range versionSet {
			legacyVersionSet[versionID] = struct{}{}
		}
		for _, attachment := range selected {
			legacyVersionSet[attachment.contentVersionID] = struct{}{}
		}
		legacyScopes, legacySources, err := legacyVersionPurgeScopesTx(
			ctx, tx, legacyVersionSet, request.All, asOf)
		if err != nil {
			return err
		}
		legacyChanges, err := installDerivativePurgeSuppressionRecordsTx(ctx, tx, legacyScopes)
		if err != nil {
			return err
		}
		suppressionChanges = append(suppressionChanges, legacyChanges...)
		purgedLegacySources := make(map[string]struct{})
		for sourceHash := range legacySources {
			purgedLegacySources[sourceHash] = struct{}{}
		}
		for buildID := range logicallyPurgedBuilds {
			var sourceHash, providerOperationID string
			if err := tx.QueryRowContext(ctx, `SELECT source_sha256,provider_operation_id
				FROM rendition_builds WHERE build_id=?`, buildID,
			).Scan(&sourceHash, &providerOperationID); err != nil {
				return fmt.Errorf("reading logically purged build %s: %w", buildID, err)
			}
			if providerOperationID == legacyPlainTextProvider {
				purgedLegacySources[sourceHash] = struct{}{}
			}
		}

		lexicalSchema, err := lexicalGenerationSchemaPresentTx(ctx, tx)
		if err != nil {
			return err
		}
		if lexicalSchema {
			headExclusions := make(map[string]struct{}, len(candidateBuilds)+len(logicallyPurgedBuilds))
			for buildID := range candidateBuilds {
				headExclusions[buildID] = struct{}{}
			}
			for buildID := range logicallyPurgedBuilds {
				headExclusions[buildID] = struct{}{}
			}
			generationRows, err := tx.QueryContext(ctx, `
				SELECT generation_id,build_id
				FROM rendition_lexical_generation_builds ORDER BY generation_id,build_id`)
			if err != nil {
				return fmt.Errorf("reading lexical generation build membership: %w", err)
			}
			defer func(rows *sql.Rows) { _ = rows.Close() }(generationRows)
			generationBuilds := make(map[string][]string)
			for generationRows.Next() {
				var generationID, buildID string
				if err := generationRows.Scan(&generationID, &buildID); err != nil {
					return fmt.Errorf("scanning lexical generation build membership: %w", err)
				}
				generationBuilds[generationID] = append(generationBuilds[generationID], buildID)
			}
			if err := generationRows.Err(); err != nil {
				return fmt.Errorf("reading lexical generation build membership: %w", err)
			}
			generationRows, err = tx.QueryContext(ctx, `
				SELECT g.generation_id,
				       EXISTS(SELECT 1 FROM rendition_lexical_heads h
				              WHERE h.generation_id=g.generation_id),
				       EXISTS(SELECT 1 FROM current_rendition_roots r
				              WHERE r.target_kind='lexical_generation'
				                AND r.target_id=g.generation_id
				                AND r.active=1
				                AND (r.expires_at IS NULL OR r.expires_at>?))
				FROM rendition_lexical_generations g ORDER BY g.generation_id`, asOf)
			if err != nil {
				return fmt.Errorf("listing lexical generations for collection: %w", err)
			}
			defer func(rows *sql.Rows) { _ = rows.Close() }(generationRows)
			type generationState struct {
				id              string
				headed          bool
				typedRooted     bool
				targetsExcluded bool
			}
			var generations []generationState
			for generationRows.Next() {
				var generation generationState
				if err := generationRows.Scan(
					&generation.id, &generation.headed, &generation.typedRooted,
				); err != nil {
					return fmt.Errorf("scanning lexical generation for collection: %w", err)
				}
				for _, buildID := range generationBuilds[generation.id] {
					if _, excluded := headExclusions[buildID]; excluded {
						generation.targetsExcluded = true
						break
					}
				}
				generations = append(generations, generation)
			}
			if err := generationRows.Err(); err != nil {
				return fmt.Errorf("listing lexical generations for collection: %w", err)
			}
			var replacementGenerationID string
			for index := range generations {
				generation := &generations[index]
				if !generation.headed || !generation.targetsExcluded {
					continue
				}
				replacement, err := stageLexicalGenerationExcludingTx(ctx, tx, headExclusions)
				if err != nil {
					return fmt.Errorf("staging lexical purge replacement: %w", err)
				}
				if replacement.ID == "" {
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM rendition_lexical_heads WHERE generation_id=?`, generation.id,
					); err != nil {
						return fmt.Errorf("revoking empty lexical generation %s head: %w", generation.id, err)
					}
				} else {
					if _, err := tx.ExecContext(ctx, `
						UPDATE rendition_lexical_heads SET generation_id=?
						WHERE singleton=1 AND generation_id=?`, replacement.ID, generation.id,
					); err != nil {
						return fmt.Errorf("publishing lexical purge replacement %s: %w",
							replacement.ID, err)
					}
					replacementGenerationID = replacement.ID
				}
				generation.headed = false
			}

			readers := lexicalGenerationReaders.stores[s]
			for _, generation := range generations {
				if generation.id == replacementGenerationID || generation.headed {
					continue
				}
				if generation.typedRooted || readers[generation.id] != 0 {
					report.RetainedLexicalGenerations = append(
						report.RetainedLexicalGenerations, generation.id)
					for _, buildID := range generationBuilds[generation.id] {
						if _, selected := candidateBuilds[buildID]; selected {
							delete(candidateBuilds, buildID)
							report.RetainedBuildIDs = append(report.RetainedBuildIDs, buildID)
						}
					}
					continue
				}
				if _, err = tx.ExecContext(ctx,
					`DELETE FROM rendition_lexical_heads WHERE generation_id=?`, generation.id,
				); err != nil {
					return fmt.Errorf("removing lexical generation %s head: %w", generation.id, err)
				}
				result, err = tx.ExecContext(ctx,
					`DELETE FROM rendition_lexical_fts WHERE generation_id=?`, generation.id)
				if err != nil {
					return fmt.Errorf("removing lexical generation %s rows: %w", generation.id, err)
				}
				count, err := rowsAffectedInt(result)
				if err != nil {
					return err
				}
				report.RemovedLexicalRows += count
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM rendition_lexical_generation_manifests WHERE generation_id=?`, generation.id,
				); err != nil {
					return fmt.Errorf("removing lexical generation %s manifest: %w", generation.id, err)
				}
				result, err = tx.ExecContext(ctx,
					`DELETE FROM rendition_lexical_generations WHERE generation_id=?`, generation.id)
				if err != nil {
					return fmt.Errorf("removing lexical generation %s: %w", generation.id, err)
				}
				count, err = rowsAffectedInt(result)
				if err != nil {
					return err
				}
				report.RemovedLexicalGenerations += count
			}
		}

		artifactBlobs := make(map[string]struct{})
		if err := func() (retErr error) {
			stagedRows, err := tx.QueryContext(ctx,
				`SELECT blob_hash FROM rendition_blob_staging ORDER BY blob_hash`)
			if err != nil {
				return fmt.Errorf("reading abandoned rendition staging: %w", err)
			}
			defer func() { retErr = errors.Join(retErr, stagedRows.Close()) }()
			for stagedRows.Next() {
				var hash string
				if err := stagedRows.Scan(&hash); err != nil {
					return fmt.Errorf("scanning abandoned rendition staging: %w", err)
				}
				artifactBlobs[hash] = struct{}{}
			}
			if err := stagedRows.Err(); err != nil {
				return fmt.Errorf("reading abandoned rendition staging: %w", err)
			}
			return nil
		}(); err != nil {
			return err
		}
		for _, buildID := range derivativeSortedKeys(candidateBuilds) {
			var sourceHash, providerOperationID string
			if err := tx.QueryRowContext(ctx,
				`SELECT source_sha256,provider_operation_id
				 FROM rendition_builds WHERE build_id=?`, buildID,
			).Scan(&sourceHash, &providerOperationID); err != nil {
				return fmt.Errorf("reading rendition build %s source: %w", buildID, err)
			}
			if providerOperationID == legacyPlainTextProvider {
				purgedLegacySources[sourceHash] = struct{}{}
			}
			blobHashes, err := stringColumnTx(ctx, tx,
				"rendition build "+buildID+" artifact blobs",
				`SELECT blob_hash FROM rendition_artifacts WHERE build_id=? ORDER BY blob_hash`, buildID)
			if err != nil {
				return err
			}
			for _, hash := range blobHashes {
				artifactBlobs[hash] = struct{}{}
			}
			children := []struct {
				query       string
				destination *int
			}{
				{`DELETE FROM rendition_lexical_segments WHERE build_id=?`, &report.RemovedLexicalSegments},
				{`DELETE FROM rendition_units WHERE build_id=?`, &report.RemovedUnits},
				{`DELETE FROM rendition_artifacts WHERE build_id=?`, &report.RemovedArtifacts},
			}
			for _, child := range children {
				result, err := tx.ExecContext(ctx, child.query, buildID)
				if err != nil {
					return fmt.Errorf("removing rendition build %s manifest: %w", buildID, err)
				}
				count, err := rowsAffectedInt(result)
				if err != nil {
					return err
				}
				*child.destination += count
			}
			result, err := tx.ExecContext(ctx,
				`DELETE FROM rendition_builds WHERE build_id=?`, buildID)
			if err != nil {
				return fmt.Errorf("removing rendition build %s: %w", buildID, err)
			}
			count, err := rowsAffectedInt(result)
			if err != nil {
				return err
			}
			report.RemovedBuilds += count
		}

		for sourceHash := range purgedLegacySources {
			suppressed, err := legacyTextExtractionSuppressedTx(ctx, tx, sourceHash)
			if err != nil {
				return fmt.Errorf("checking shared legacy purge scope for %s: %w", sourceHash, err)
			}
			if !suppressed {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts WHERE rowid IN (
				SELECT rowid FROM extracted_text WHERE blob_hash=?
			)`, sourceHash); err != nil {
				return fmt.Errorf("removing legacy lexical cache for %s: %w", sourceHash, err)
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM extracted_text WHERE blob_hash=?`, sourceHash)
			if err != nil {
				return fmt.Errorf("removing legacy cache for %s: %w", sourceHash, err)
			}
			count, err := rowsAffectedInt(result)
			if err != nil {
				return err
			}
			report.RemovedLegacyCacheRows += count
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM text_extraction_queue WHERE blob_hash=?`, sourceHash,
			); err != nil {
				return fmt.Errorf("removing legacy cache work for %s: %w", sourceHash, err)
			}
		}

		for hash := range artifactBlobs {
			var reachable bool
			if err := tx.QueryRowContext(ctx, `SELECT
				EXISTS(SELECT 1 FROM content_versions WHERE blob_hash=?) OR
				EXISTS(SELECT 1 FROM rendition_artifacts WHERE blob_hash=?) OR
				EXISTS(SELECT 1 FROM rendition_builds WHERE source_sha256=?)`,
				hash, hash, hash).Scan(&reachable); err != nil {
				return fmt.Errorf("checking purged derivative blob %s reachability: %w", hash, err)
			}
			if !reachable {
				if _, err := tx.ExecContext(ctx, `INSERT INTO derivative_blob_purge_pending(blob_hash)
					VALUES(?) ON CONFLICT(blob_hash) DO NOTHING`, hash); err != nil {
					return fmt.Errorf("persisting derivative blob purge target %s: %w", hash, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM derivative_blob_purge_pending
			WHERE EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash=derivative_blob_purge_pending.blob_hash)
			   OR EXISTS (SELECT 1 FROM rendition_artifacts a WHERE a.blob_hash=derivative_blob_purge_pending.blob_hash)
			   OR EXISTS (SELECT 1 FROM rendition_builds r WHERE r.source_sha256=derivative_blob_purge_pending.blob_hash)`); err != nil {
			return fmt.Errorf("reconciling derivative blob purge targets: %w", err)
		}
		if err := func() (retErr error) {
			pendingRows, err := tx.QueryContext(ctx,
				`SELECT blob_hash FROM derivative_blob_purge_pending ORDER BY blob_hash`)
			if err != nil {
				return fmt.Errorf("reading derivative blob purge targets: %w", err)
			}
			defer func() { retErr = errors.Join(retErr, pendingRows.Close()) }()
			for pendingRows.Next() {
				var hash string
				if err := pendingRows.Scan(&hash); err != nil {
					return fmt.Errorf("scanning derivative blob purge target: %w", err)
				}
				report.PhysicalDerivativeBlobsPendingGC = append(
					report.PhysicalDerivativeBlobsPendingGC, hash)
			}
			if err := pendingRows.Err(); err != nil {
				return fmt.Errorf("reading derivative blob purge targets: %w", err)
			}
			return nil
		}(); err != nil {
			return err
		}
		sort.Strings(report.PhysicalDerivativeBlobsPendingGC)
		sort.Strings(report.RetainedBuildIDs)
		report.RetainedBuildIDs = slices.Compact(report.RetainedBuildIDs)
		sort.Strings(report.RetainedLexicalGenerations)
		if auditActive && len(suppressionChanges) != 0 {
			if err := s.persistAuditedDerivativeSuppressionChanges(
				ctx, tx, suppressionChanges); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PurgeReport{}, fmt.Errorf("purging derivatives: %w", err)
	}
	return report, nil
}

func legacyVersionPurgeScopesTx(
	ctx context.Context, tx *sql.Tx, versionSet map[string]struct{}, all bool, purgedAt string,
) (_ []derivativePurgeSuppression, sources map[string]struct{}, retErr error) {
	if !all && len(versionSet) == 0 {
		return nil, map[string]struct{}{}, nil
	}
	profile, err := legacyPlainTextProfile()
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT v.version_id,v.blob_hash,e.extractor,e.extractor_version,e.status,e.text
		FROM content_versions v
		LEFT JOIN extracted_text e ON e.blob_hash=v.blob_hash AND e.extractor=?
		ORDER BY v.version_id`, legacyPlainTextExtractor)
	if err != nil {
		return nil, nil, fmt.Errorf("reading legacy versions for derivative purge: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	sources = make(map[string]struct{})
	seen := make(map[string]struct{})
	var scopes []derivativePurgeSuppression
	for rows.Next() {
		var versionID, blobHash string
		var extractor, status sql.NullString
		var extractorVersion sql.NullInt64
		var text sql.NullString
		if err := rows.Scan(&versionID, &blobHash, &extractor, &extractorVersion, &status, &text); err != nil {
			return nil, nil, fmt.Errorf("scanning legacy version for derivative purge: %w", err)
		}
		if _, selected := versionSet[versionID]; !all && !selected {
			continue
		}
		buildID := legacyPendingPurgeFingerprint(blobHash, versionID, profile.Fingerprint)
		if extractor.Valid && extractorVersion.Valid && status.Valid &&
			extractorVersion.Int64 == legacyPlainTextExtractorVersion &&
			status.String == ExtractionOK && text.Valid && utf8.ValidString(text.String) {
			buildID = legacyPlainTextBuildFingerprint(
				blobHash, extractor.String, extractorVersion.Int64, status.String, []byte(text.String))
		}
		scope := derivativePurgeSuppression{
			sourceSHA256: blobHash,
			profileFingerprint: derivativeAttachmentSuppressionScope(
				versionID, profile.Fingerprint),
			buildID: buildID, purgedAt: purgedAt, active: true,
		}
		key := scope.sourceSHA256 + scope.profileFingerprint + scope.buildID
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			scopes = append(scopes, scope)
		}
		sources[blobHash] = struct{}{}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM text_searchable_versions WHERE version_id=?`, versionID); err != nil {
			return nil, nil, fmt.Errorf("revoking legacy searchable version %s: %w", versionID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading legacy versions for derivative purge: %w", err)
	}
	return scopes, sources, nil
}

func legacyPendingPurgeFingerprint(sourceSHA256, versionID, profileFingerprint string) string {
	digest := sha256.Sum256([]byte("docbank:legacy-pending-purge:v1\x00" +
		sourceSHA256 + "\x00" + versionID + "\x00" + profileFingerprint))
	return hex.EncodeToString(digest[:])
}

func validatePurgeRequest(request PurgeRequest) error {
	const maxPurgeIDs = 1000
	if len(request.ContentVersionIDs) > maxPurgeIDs || len(request.AttachmentIDs) > maxPurgeIDs ||
		len(request.BuildIDs) > maxPurgeIDs {
		return fmt.Errorf("derivative purge accepts at most %d IDs per selector", maxPurgeIDs)
	}
	selectors := []struct {
		subject string
		values  []string
	}{
		{"content version", request.ContentVersionIDs},
		{"attachment", request.AttachmentIDs},
		{"build", request.BuildIDs},
	}
	for _, selector := range selectors {
		subject, values := selector.subject, selector.values
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if value == "" || len(value) > maxCatalogIdentifierBytes {
				return fmt.Errorf("derivative purge %s ID is invalid", subject)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("derivative purge %s ID %q is duplicated", subject, value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func derivativeSortedKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func rowsAffectedInt(result sql.Result) (int, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count < 0 || count > math.MaxInt {
		return 0, errors.New("SQLite affected-row count exceeds int")
	}
	return int(count), nil
}

func lexicalGenerationSchemaPresentTx(ctx context.Context, tx metadataQuerier) (bool, error) {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_schema
		WHERE type='table' AND name='rendition_lexical_generations'
	)`).Scan(&present); err != nil {
		return false, fmt.Errorf("checking lexical generation schema: %w", err)
	}
	return present, nil
}

// PutCurrentRenditionRoot creates or renews one exact root. A higher fencing
// token supersedes prior authority; an equal record is an idempotent replay.
func (s *Store) PutCurrentRenditionRoot(ctx context.Context, root CurrentRenditionRoot) error {
	if err := validateCurrentRenditionRoot(root); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return putCurrentRenditionRootTx(ctx, tx, root)
	})
}

func putCurrentRenditionRootTx(
	ctx context.Context, tx *sql.Tx, root CurrentRenditionRoot,
) error {
	var stored CurrentRenditionRoot
	var storedActive bool
	err := tx.QueryRowContext(ctx, `
			SELECT root_id,root_kind,target_kind,target_id,fencing_token,recorded_at,
			       COALESCE(expires_at,''),active
			FROM current_rendition_roots WHERE root_id=?`, root.ID,
	).Scan(&stored.ID, &stored.Kind, &stored.TargetKind, &stored.TargetID,
		&stored.FencingToken, &stored.RecordedAt, &stored.ExpiresAt, &storedActive)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading current rendition root %s: %w", root.ID, err)
	}
	if err == nil {
		if root.FencingToken < stored.FencingToken {
			return fmt.Errorf("root %s token %d is older than %d: %w", root.ID,
				root.FencingToken, stored.FencingToken, ErrCurrentRenditionRootFenced)
		}
		if root.FencingToken == stored.FencingToken {
			if !storedActive {
				return fmt.Errorf("root %s token %d was already released: %w",
					root.ID, root.FencingToken, ErrCurrentRenditionRootFenced)
			}
			if root == stored {
				return nil
			}
			return fmt.Errorf("root %s token %d names different authority: %w",
				root.ID, root.FencingToken, ErrCurrentRenditionRootFenced)
		}
	}
	if err := requireCurrentRenditionTargetTx(ctx, tx, root); err != nil {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
				INSERT INTO current_rendition_roots(
					root_id,root_kind,target_kind,target_id,fencing_token,recorded_at,expires_at,
					active,released_at
				) VALUES(?,?,?,?,?,?,NULLIF(?,''),1,NULL)`, root.ID, root.Kind, root.TargetKind,
			root.TargetID, root.FencingToken, root.RecordedAt, root.ExpiresAt)
		if err != nil {
			return fmt.Errorf("recording current rendition root %s: %w", root.ID, err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
			UPDATE current_rendition_roots
			SET root_kind=?,target_kind=?,target_id=?,fencing_token=?,recorded_at=?,
			    expires_at=NULLIF(?,''),active=1,released_at=NULL
			WHERE root_id=?`, root.Kind, root.TargetKind, root.TargetID,
		root.FencingToken, root.RecordedAt, root.ExpiresAt, root.ID); err != nil {
		return fmt.Errorf("renewing current rendition root %s: %w", root.ID, err)
	}
	return nil
}

// ReleaseCurrentRenditionRoot releases only the exact fencing token supplied.
// A stale release is an idempotent no-op and cannot revoke renewed authority.
func (s *Store) ReleaseCurrentRenditionRoot(
	ctx context.Context, rootID string, fencingToken int64,
) (bool, error) {
	if rootID == "" || len(rootID) > maxCatalogIdentifierBytes || fencingToken <= 0 {
		return false, errors.New("current rendition root release is invalid")
	}
	var released bool
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE current_rendition_roots SET active=0,released_at=?
			WHERE root_id=? AND fencing_token=? AND active=1`,
			nowRFC3339(), rootID, fencingToken)
		if err != nil {
			return fmt.Errorf("releasing current rendition root %s: %w", rootID, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting released current rendition root %s: %w", rootID, err)
		}
		released = count != 0
		return nil
	})
	return released, err
}

func validateCurrentRenditionRoot(root CurrentRenditionRoot) error {
	if root.ID == "" || len(root.ID) > maxCatalogIdentifierBytes ||
		root.TargetID == "" || len(root.TargetID) > maxCatalogIdentifierBytes ||
		root.FencingToken <= 0 {
		return errors.New("current rendition root identity and fencing token are required")
	}
	switch root.Kind {
	case RenditionRootAttachment, RenditionRootHead, RenditionRootRetention,
		RenditionRootAudit, RenditionRootJob, RenditionRootBackupPin:
		if root.ExpiresAt != "" {
			return errors.New("non-lease current rendition root must not expire")
		}
	case RenditionRootReaderLease, RenditionRootWorkerLease:
		if root.ExpiresAt == "" {
			return errors.New("current rendition lease requires an expiry")
		}
		if err := validateMetadataTime("current rendition root expires_at", root.ExpiresAt); err != nil {
			return err
		}
	default:
		return fmt.Errorf("current rendition root kind %q is invalid", root.Kind)
	}
	switch root.TargetKind {
	case RenditionRootBuild, RenditionRootLexicalGeneration:
	default:
		return fmt.Errorf("current rendition target kind %q is invalid", root.TargetKind)
	}
	return validateMetadataTime("current rendition root recorded_at", root.RecordedAt)
}

func requireCurrentRenditionTargetTx(
	ctx context.Context, tx *sql.Tx, root CurrentRenditionRoot,
) error {
	var present bool
	switch root.TargetKind {
	case RenditionRootBuild:
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM rendition_builds WHERE build_id=?)`, root.TargetID,
		).Scan(&present); err != nil {
			return fmt.Errorf("checking current rendition build root: %w", err)
		}
	case RenditionRootLexicalGeneration:
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM sqlite_schema WHERE type='table'
			AND name='rendition_lexical_generations'
		)`).Scan(&present); err != nil {
			return fmt.Errorf("checking lexical generation schema: %w", err)
		}
		if present {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM rendition_lexical_generations WHERE generation_id=?
			)`, root.TargetID).Scan(&present); err != nil {
				return fmt.Errorf("checking current lexical generation root: %w", err)
			}
		}
	}
	if !present {
		return fmt.Errorf("current rendition root target %s: %w", root.TargetID, ErrNotFound)
	}
	return nil
}

// DerivativeGCPlan returns the complete currently-unrooted derivative set.
func (s *Store) DerivativeGCPlan(ctx context.Context) (_ DerivativeGCPlan, retErr error) {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("starting derivative GC snapshot: %w", err)
	}
	active := true
	defer func() {
		if !active {
			return
		}
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()

	asOf := nowRFC3339()
	rows, err := tx.QueryContext(ctx, `
		SELECT b.build_id,a.blob_hash
		FROM rendition_builds b
		LEFT JOIN rendition_artifacts a ON a.build_id=b.build_id
		WHERE NOT EXISTS (
			SELECT 1 FROM rendition_attachments x WHERE x.build_id=b.build_id
		)
		AND NOT EXISTS (
			SELECT 1 FROM current_rendition_roots r
			WHERE r.target_kind='rendition_build' AND r.target_id=b.build_id
			  AND r.active=1
			  AND (r.expires_at IS NULL OR r.expires_at>?)
		)
		ORDER BY b.build_id,a.blob_hash`, asOf)
	if err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("planning derivative builds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	plan := DerivativeGCPlan{Builds: []DerivativeBuildGC{}, LexicalGenerations: []string{}}
	for rows.Next() {
		var buildID string
		var blobHash sql.NullString
		if err := rows.Scan(&buildID, &blobHash); err != nil {
			return DerivativeGCPlan{}, fmt.Errorf("scanning derivative build candidate: %w", err)
		}
		if len(plan.Builds) == 0 || plan.Builds[len(plan.Builds)-1].BuildID != buildID {
			plan.Builds = append(plan.Builds, DerivativeBuildGC{BuildID: buildID})
		}
		if blobHash.Valid {
			plan.Builds[len(plan.Builds)-1].ArtifactBlobHashes = append(
				plan.Builds[len(plan.Builds)-1].ArtifactBlobHashes, blobHash.String,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("planning derivative builds: %w", err)
	}
	if err := rows.Close(); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("closing derivative build candidates: %w", err)
	}

	var lexicalSchema bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_schema
		WHERE type='table' AND name='rendition_lexical_generations'
	)`).Scan(&lexicalSchema); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("checking lexical generation schema: %w", err)
	}
	if lexicalSchema {
		generationRows, err := tx.QueryContext(ctx, `
			SELECT g.generation_id,
			       EXISTS(SELECT 1 FROM rendition_lexical_heads h
			              WHERE h.generation_id=g.generation_id),
			       EXISTS(SELECT 1 FROM current_rendition_roots r
			              WHERE r.target_kind='lexical_generation'
			                AND r.target_id=g.generation_id
			                AND r.active=1
			                AND (r.expires_at IS NULL OR r.expires_at>?))
			FROM rendition_lexical_generations g ORDER BY g.generation_id`, asOf)
		if err != nil {
			return DerivativeGCPlan{}, fmt.Errorf("planning lexical generations: %w", err)
		}
		defer func() { _ = generationRows.Close() }()
		readers := lexicalGenerationReaders.stores[s]
		var rootedGenerationIDs []string
		for generationRows.Next() {
			var generationID string
			var headed, rooted bool
			if err := generationRows.Scan(&generationID, &headed, &rooted); err != nil {
				return DerivativeGCPlan{}, fmt.Errorf("scanning lexical generation candidate: %w", err)
			}
			if headed || rooted || readers[generationID] != 0 {
				rootedGenerationIDs = append(rootedGenerationIDs, generationID)
			} else {
				plan.LexicalGenerations = append(plan.LexicalGenerations, generationID)
			}
		}
		if err := generationRows.Err(); err != nil {
			return DerivativeGCPlan{}, fmt.Errorf("planning lexical generations: %w", err)
		}
		if err := generationRows.Close(); err != nil {
			return DerivativeGCPlan{}, fmt.Errorf("closing lexical generation candidates: %w", err)
		}
		rootedBuilds := make(map[string]struct{})
		for _, generationID := range rootedGenerationIDs {
			buildIDs, err := stringColumnTx(ctx, tx,
				"rooted lexical generation "+generationID, `
				SELECT build_id FROM rendition_lexical_generation_builds
				WHERE generation_id=? ORDER BY build_id`, generationID)
			if err != nil {
				return DerivativeGCPlan{}, err
			}
			for _, buildID := range buildIDs {
				rootedBuilds[buildID] = struct{}{}
			}
		}
		if len(rootedBuilds) != 0 {
			candidates := plan.Builds[:0]
			for _, build := range plan.Builds {
				if _, rooted := rootedBuilds[build.BuildID]; !rooted {
					candidates = append(candidates, build)
				}
			}
			plan.Builds = candidates
		}
	}
	expiredRows, err := tx.QueryContext(ctx, `
		SELECT root_id FROM current_rendition_roots
		WHERE active=1 AND expires_at IS NOT NULL AND expires_at<=? ORDER BY root_id`, asOf)
	if err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("planning expired current rendition roots: %w", err)
	}
	defer func() { _ = expiredRows.Close() }()
	for expiredRows.Next() {
		var rootID string
		if err := expiredRows.Scan(&rootID); err != nil {
			return DerivativeGCPlan{}, fmt.Errorf("scanning expired current rendition root: %w", err)
		}
		plan.ExpiredRootIDs = append(plan.ExpiredRootIDs, rootID)
	}
	if err := expiredRows.Err(); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("planning expired current rendition roots: %w", err)
	}
	if err := expiredRows.Close(); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("closing expired current rendition roots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DerivativeGCPlan{}, fmt.Errorf("closing derivative GC snapshot: %w", err)
	}
	active = false
	return plan, nil
}
func pageLimitWithSentinel(limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("page limit must be positive")
	}
	if limit == math.MaxInt {
		return 0, errors.New("page limit is too large")
	}
	return limit + 1, nil
}

// BlobInfo returns logical catalog membership independently of whether a
// loose or packed representation currently has physical read authority.
func (s *Store) BlobInfo(ctx context.Context, hash string) (BlobInfo, error) {
	var info BlobInfo
	err := s.db.QueryRowContext(ctx,
		`SELECT hash, size FROM blobs WHERE hash = ?`, hash,
	).Scan(&info.Hash, &info.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobInfo{}, ErrNotFound
	}
	if err != nil {
		return BlobInfo{}, fmt.Errorf("reading blob membership %s: %w", hash, err)
	}
	return info, nil
}

// GCCandidate is one unreachable catalog row and its indexed loose authority.
type GCCandidate struct {
	Hash            string
	Loose           bool
	LooseStoredSize int64
}

// GCCandidateScanPage reports bounded raw catalog progress independently of
// how many rows qualify as unreachable work.
type GCCandidateScanPage struct {
	Items     []GCCandidate
	Examined  int
	HighWater string
	More      bool
}

// StringScanPage reports bounded raw key progress for a filtered string
// inventory such as unreferenced packed mappings.
type StringScanPage struct {
	Items     []string
	Examined  int
	HighWater string
	More      bool
}

// RepackCandidate binds one sparse pack to the lowest canonical live blob hash
// that provides its stable maintenance key.
type RepackCandidate struct {
	Hash     string
	Usage    packstore.PackUsage
	Eligible bool
}

type RepackScanPage struct {
	Items []RepackCandidate
	More  bool
}

// HasBlob reports whether the metadata catalog grants authority to hash.
func (s *Store) HasBlob(ctx context.Context, hash string) (bool, error) {
	var recorded bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`, hash,
	).Scan(&recorded); err != nil {
		return false, fmt.Errorf("checking blob authority for %s: %w", hash, err)
	}
	return recorded, nil
}

// HasPrimaryLooseAuthority reports whether the built-in primary catalog still
// authorizes a canonical loose representation for hash. Physical scans use
// this narrower question so redundant files left by packing, placement, or a
// remote-only restore can be reclaimed without deleting logical membership.
func (s *Store) HasPrimaryLooseAuthority(ctx context.Context, hash string) (bool, error) {
	var recorded bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM blob_locations
			WHERE blob_hash=? AND store_id=? AND kind='loose'
		)`, hash, s.primaryStoreID).Scan(&recorded); err != nil {
		return false, fmt.Errorf("checking primary loose authority for %s: %w", hash, err)
	}
	return recorded, nil
}

func scanBlobInfos(rows *sql.Rows, op string) ([]BlobInfo, error) {
	defer func() { _ = rows.Close() }()
	var out []BlobInfo
	for rows.Next() {
		var b BlobInfo
		if err := rows.Scan(&b.Hash, &b.Size); err != nil {
			return nil, fmt.Errorf("%s: scanning blob row: %w", op, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// UnreachableBlobs lists blobs referenced by no original content version or
// retained rendition artifact. Every current file head is itself a content
// version, as are retained prior versions. These are the gc candidates. Callers that go
// on to delete blob files must serialize against concurrent writers (the
// daemon's maintenance gate does this): with writers running, a concurrent
// ingest can dedup against a candidate's file between this query and the
// deletion, leaving a live node pointing at a removed blob.
func (s *Store) UnreachableBlobs(ctx context.Context) ([]BlobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.hash, b.size FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = b.hash)
		  AND NOT EXISTS (SELECT 1 FROM rendition_builds r WHERE r.source_sha256 = b.hash)
		  AND NOT EXISTS (SELECT 1 FROM rendition_artifacts a WHERE a.blob_hash = b.hash)
		ORDER BY b.hash`)
	if err != nil {
		return nil, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	return scanBlobInfos(rows, "finding unreachable blobs")
}

// UnreachableBlobsPageFrom distinguishes the beginning of an ordering from an
// arbitrary stored key, including the empty string. It examines a bounded raw
// key window before filtering for unreachable work.
func (s *Store) UnreachableBlobsPageFrom(
	ctx context.Context, after *string, limit int,
) (GCCandidateScanPage, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return GCCandidateScanPage{}, fmt.Errorf("blob page limit: %w", err)
	}
	query, args := unreachableBlobScanQuery(after, queryLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return GCCandidateScanPage{}, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scanRow struct {
		candidate GCCandidate
		eligible  bool
	}
	raw := make([]scanRow, 0, queryLimit)
	for rows.Next() {
		var item scanRow
		var looseSize sql.NullInt64
		if err := rows.Scan(&item.candidate.Hash, &looseSize, &item.eligible); err != nil {
			return GCCandidateScanPage{},
				fmt.Errorf("finding unreachable blobs: scanning blob row: %w", err)
		}
		item.candidate.Loose = looseSize.Valid
		item.candidate.LooseStoredSize = looseSize.Int64
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return GCCandidateScanPage{}, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	more := len(raw) > limit
	if more {
		raw = raw[:limit]
	}
	page := GCCandidateScanPage{Examined: len(raw), More: more}
	if len(raw) > 0 {
		page.HighWater = raw[len(raw)-1].candidate.Hash
	}
	for _, item := range raw {
		if item.eligible {
			page.Items = append(page.Items, item.candidate)
		}
	}
	return page, nil
}

const unreachableBlobsStartPageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT b.hash, l.stored_size AS loose_stored_size
		FROM blobs b
		LEFT JOIN blob_locations l
		  ON l.blob_hash = b.hash
		 AND l.store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
		 AND l.kind = 'loose'
		ORDER BY b.hash LIMIT ?
	)
	SELECT p.hash, p.loose_stored_size,
	       NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = p.hash)
	       AND NOT EXISTS (SELECT 1 FROM rendition_builds r WHERE r.source_sha256 = p.hash)
	       AND NOT EXISTS (SELECT 1 FROM rendition_artifacts a WHERE a.blob_hash = p.hash)
	FROM raw_page p ORDER BY p.hash`

const unreachableBlobsResumePageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT b.hash, l.stored_size AS loose_stored_size
		FROM blobs b
		LEFT JOIN blob_locations l
		  ON l.blob_hash = b.hash
		 AND l.store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
		 AND l.kind = 'loose'
		WHERE b.hash > ? ORDER BY b.hash LIMIT ?
	)
	SELECT p.hash, p.loose_stored_size,
	       NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = p.hash)
	       AND NOT EXISTS (SELECT 1 FROM rendition_builds r WHERE r.source_sha256 = p.hash)
	       AND NOT EXISTS (SELECT 1 FROM rendition_artifacts a WHERE a.blob_hash = p.hash)
	FROM raw_page p ORDER BY p.hash`

func unreachableBlobScanQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return unreachableBlobsStartPageSQL, []any{limit}
	}
	return unreachableBlobsResumePageSQL, []any{*after, limit}
}

// BlobsPage returns one bounded hash-keyset page of recorded blob identities.
func (s *Store) BlobsPage(ctx context.Context, after string, limit int) ([]BlobInfo, bool, error) {
	return s.blobPage(ctx, `
		SELECT hash, size FROM blobs WHERE hash > ? ORDER BY hash LIMIT ?`,
		after, limit, "listing blobs")
}

// BlobHashesPage is the scalar-tolerant verification inventory. It does not
// scan ancillary blob metadata, so a malformed size remains reportable by
// ValidateMetadata without suppressing content verification.
func (s *Store) BlobHashesPage(
	ctx context.Context, after string, limit int,
) ([]string, bool, error) {
	return s.BlobHashesPageFrom(ctx, &after, limit)
}

// BlobHashesPageFrom distinguishes the beginning of an ordering from an
// arbitrary stored key, including the empty string.
func (s *Store) BlobHashesPageFrom(
	ctx context.Context, after *string, limit int,
) ([]string, bool, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return nil, false, fmt.Errorf("blob hash page limit: %w", err)
	}
	query, args := blobHashesPageQuery(after, queryLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("listing blob hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0, queryLimit)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, false, fmt.Errorf("scanning blob hash: %w", err)
		}
		result = append(result, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("listing blob hashes: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

const blobHashesStartPageSQL = `SELECT hash FROM blobs ORDER BY hash LIMIT ?`
const blobHashesResumePageSQL = `SELECT hash FROM blobs WHERE hash > ? ORDER BY hash LIMIT ?`

func blobHashesPageQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return blobHashesStartPageSQL, []any{limit}
	}
	return blobHashesResumePageSQL, []any{*after, limit}
}

func (s *Store) blobPage(
	ctx context.Context, query, after string, limit int, operation string,
) ([]BlobInfo, bool, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return nil, false, fmt.Errorf("blob page limit: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, query, after, queryLimit)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", operation, err)
	}
	items, err := scanBlobInfos(rows, operation)
	if err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

// SparseRepackPage returns eligible non-empty sparse packs ordered by their
// unique lowest live canonical blob hash.
func (s *Store) SparseRepackPage(
	ctx context.Context,
	after string,
	limit int,
	now time.Time,
	minAge time.Duration,
	minDeadBytes int64,
) ([]RepackCandidate, bool, error) {
	page, err := s.SparseRepackScanPage(ctx, after, "\xff", limit, now, minAge, minDeadBytes)
	if err != nil {
		return nil, false, err
	}
	result := make([]RepackCandidate, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Eligible {
			result = append(result, item)
		}
	}
	return result, page.More, nil
}

const sparseRepackScanPageSQL = `
	SELECT scan_hash, pack_id, entry_count, stored_bytes, created_at,
	       live_entries, live_stored_bytes, live_raw_bytes,
	       max_live_stored_len, max_live_raw_len
	FROM blob_packs INDEXED BY blob_packs_live_scan
	WHERE store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
	  AND live_entries > 0
	  AND (scan_hash > ? OR (scan_hash = ? AND pack_id > ?))
	ORDER BY scan_hash, pack_id LIMIT ?`

// SparseRepackScanPage examines at most limit persisted pack summaries. Packs
// that do not satisfy the caller's thresholds still consume the finite scan
// budget, so selection work is independent of total catalog cardinality.
func (s *Store) SparseRepackScanPage(
	ctx context.Context,
	afterHash string,
	afterPackID string,
	limit int,
	now time.Time,
	minAge time.Duration,
	minDeadBytes int64,
) (RepackScanPage, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return RepackScanPage{}, fmt.Errorf("repack page limit: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, sparseRepackScanPageSQL,
		afterHash, afterHash, afterPackID, queryLimit)
	if err != nil {
		return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]RepackCandidate, 0, queryLimit)
	for rows.Next() {
		var candidate RepackCandidate
		var created string
		if err := rows.Scan(&candidate.Hash, &candidate.Usage.PackID,
			&candidate.Usage.EntryCount, &candidate.Usage.StoredBytes, &created,
			&candidate.Usage.LiveEntries, &candidate.Usage.LiveStoredBytes,
			&candidate.Usage.LiveRawBytes, &candidate.Usage.MaxLiveStoredLen,
			&candidate.Usage.MaxLiveRawLen); err != nil {
			return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidate: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return RepackScanPage{}, fmt.Errorf("parsing blob pack %s creation time: %w",
				candidate.Usage.PackID, err)
		}
		candidate.Usage.CreatedAt = createdAt
		candidate.Eligible = candidate.Usage.LiveEntries <= candidate.Usage.EntryCount/2 &&
			!candidate.Usage.CreatedAt.After(now.UTC().Add(-minAge)) &&
			candidate.Usage.StoredBytes-candidate.Usage.LiveStoredBytes >= minDeadBytes
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidates: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return RepackScanPage{Items: result, More: more}, nil
}

// UnreferencedPackMappingsPage returns one canonical-hash keyset page of pack
// mappings whose blob authority has been revoked.
func (s *Store) UnreferencedPackMappingsPage(
	ctx context.Context, after *string, limit int,
) (StringScanPage, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return StringScanPage{}, fmt.Errorf("pack mapping page limit: %w", err)
	}
	query, args := unreferencedMappingScanQuery(after, queryLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return StringScanPage{}, fmt.Errorf("listing unreferenced pack mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scanRow struct {
		hash     string
		eligible bool
	}
	raw := make([]scanRow, 0, queryLimit)
	for rows.Next() {
		var item scanRow
		if err := rows.Scan(&item.hash, &item.eligible); err != nil {
			return StringScanPage{}, fmt.Errorf("scanning unreferenced pack mapping: %w", err)
		}
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return StringScanPage{}, fmt.Errorf("listing unreferenced pack mappings: %w", err)
	}
	more := len(raw) > limit
	if more {
		raw = raw[:limit]
	}
	page := StringScanPage{Examined: len(raw), More: more}
	if len(raw) > 0 {
		page.HighWater = raw[len(raw)-1].hash
	}
	for _, item := range raw {
		if item.eligible {
			page.Items = append(page.Items, item.hash)
		}
	}
	return page, nil
}

const unreferencedMappingsStartPageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT blob_hash FROM blob_pack_entries
		WHERE store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
		ORDER BY blob_hash LIMIT ?
	)
	SELECT p.blob_hash,
	       NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = p.blob_hash)
	FROM raw_page p ORDER BY p.blob_hash`

const unreferencedMappingsResumePageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT blob_hash FROM blob_pack_entries
		WHERE store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
		  AND blob_hash > ? ORDER BY blob_hash LIMIT ?
	)
	SELECT p.blob_hash,
	       NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = p.blob_hash)
	FROM raw_page p ORDER BY p.blob_hash`

func unreferencedMappingScanQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return unreferencedMappingsStartPageSQL, []any{limit}
	}
	return unreferencedMappingsResumePageSQL, []any{*after, limit}
}

// DeleteUnreferencedPackMappings conditionally removes the named stale
// mappings. A blob authority restored after selection protects its mapping.
func (s *Store) DeleteUnreferencedPackMappings(ctx context.Context, hashes []string) (int64, error) {
	var removed int64
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, hash := range hashes {
			result, err := tx.ExecContext(ctx, `
				DELETE FROM blob_pack_entries
				WHERE blob_hash = ?
				  AND store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
				  AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = ?)`, hash, hash)
			if err != nil {
				return fmt.Errorf("deleting unreferenced pack mapping %s: %w", hash, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting deleted pack mapping %s: %w", hash, err)
			}
			removed += count
		}
		return nil
	})
	return removed, err
}

// DeadPackUsagePage returns a bounded set of packs with no live mappings.
// Successful repack retirement deletes each returned candidate, so callers can
// resume this phase without an identity cursor.
const deadPackUsagePageSQL = `
	SELECT pack_id, entry_count, stored_bytes, created_at,
	       live_entries, live_stored_bytes, live_raw_bytes,
	       max_live_stored_len, max_live_raw_len
	FROM blob_packs INDEXED BY blob_packs_dead_scan
	WHERE store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
	  AND live_entries = 0
	ORDER BY scan_hash, pack_id LIMIT ?`

func (s *Store) DeadPackUsagePage(
	ctx context.Context, limit int,
) ([]packstore.PackUsage, bool, error) {
	queryLimit, err := pageLimitWithSentinel(limit)
	if err != nil {
		return nil, false, fmt.Errorf("dead-pack page limit: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, deadPackUsagePageSQL, queryLimit)
	if err != nil {
		return nil, false, fmt.Errorf("listing dead packs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]packstore.PackUsage, 0, queryLimit)
	for rows.Next() {
		var usage packstore.PackUsage
		var created string
		if err := rows.Scan(&usage.PackID, &usage.EntryCount, &usage.StoredBytes, &created,
			&usage.LiveEntries, &usage.LiveStoredBytes, &usage.LiveRawBytes,
			&usage.MaxLiveStoredLen, &usage.MaxLiveRawLen); err != nil {
			return nil, false, fmt.Errorf("scanning dead pack: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, false, fmt.Errorf("parsing blob pack %s creation time: %w", usage.PackID, err)
		}
		usage.CreatedAt = createdAt
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("listing dead packs: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

// DeleteBlobRows removes logical membership and derived metadata for reclaimed
// blobs. Callers must hold the exclusive vault lock (see UnreachableBlobs),
// resolve and validate every loose location first, and retire physical bytes
// only after this logical deletion succeeds. Packed entries remain as dead
// physical accounting until repack retires their immutable container.
func (s *Store) DeleteBlobRows(ctx context.Context, hashes []string) error {
	return s.deleteBlobRows(ctx, hashes, nil, nil)
}

// DeleteBlobRowsWithGCRetirements atomically records exact physical cleanup
// before removing the blob rows that own their ordinary location metadata.
func (s *Store) DeleteBlobRowsWithGCRetirements(
	ctx context.Context,
	hashes []string,
	looseRetirements []GCLooseRetirement,
	packRetirements []GCPackRetirement,
) error {
	return s.deleteBlobRows(ctx, hashes, looseRetirements, packRetirements)
}

func (s *Store) deleteBlobRows(
	ctx context.Context,
	hashes []string,
	looseRetirements []GCLooseRetirement,
	packRetirements []GCPackRetirement,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, retirement := range looseRetirements {
			if retirement.StoreID == "" {
				return errors.New("GC loose retirement store ID is required")
			}
			if err := retirement.Hash.Validate(); err != nil {
				return fmt.Errorf("validating GC loose retirement hash: %w", err)
			}
			if retirement.Encoding != packstore.LooseEncodingRaw &&
				retirement.Encoding != packstore.LooseEncodingZstd {
				return fmt.Errorf("invalid GC loose retirement encoding %d", retirement.Encoding)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO gc_loose_retirements(
				store_id,blob_hash,loose_encoding
			) VALUES(?,?,?)`, retirement.StoreID, retirement.Hash.String(), retirement.Encoding); err != nil {
				return fmt.Errorf("recording GC loose retirement: %w", err)
			}
		}
		for _, retirement := range packRetirements {
			if retirement.StoreID == "" || retirement.PackID == "" {
				return errors.New("GC pack retirement identity is required")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO derivative_pack_purge_pending(
				store_id,pack_id
			) VALUES(?,?)`, retirement.StoreID, retirement.PackID); err != nil {
				return fmt.Errorf("recording derivative pack retirement: %w", err)
			}
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx, `DELETE FROM text_extraction_queue WHERE blob_hash = ?`, h); err != nil {
				return fmt.Errorf("deleting extraction queue row of %s: %w", h, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts WHERE rowid IN (
				SELECT rowid FROM extracted_text WHERE blob_hash = ?
			)`, h); err != nil {
				return fmt.Errorf("deleting content search rows of %s: %w", h, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM extracted_text WHERE blob_hash = ?`, h); err != nil {
				return fmt.Errorf("deleting extracted text of %s: %w", h, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, h); err != nil {
				return fmt.Errorf("deleting blob row %s: %w", h, err)
			}
		}
		return nil
	})
}

// PendingDerivativePackRetirements returns a bounded stable batch of packs
// that exact derivative erasure must rewrite regardless of ordinary sparsity.
func (s *Store) PendingDerivativePackRetirements(
	ctx context.Context, limit int,
) ([]packstore.PackUsage, error) {
	if limit <= 0 {
		return nil, errors.New("derivative pack retirement limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.pack_id,p.entry_count,p.stored_bytes,p.created_at,
		       p.live_entries,p.live_stored_bytes,p.live_raw_bytes,
		       p.max_live_stored_len,p.max_live_raw_len
		FROM derivative_pack_purge_pending r
		JOIN blob_packs p ON p.store_id=r.store_id AND p.pack_id=r.pack_id
		WHERE r.store_id=(SELECT store_id FROM blob_stores WHERE role='primary')
		ORDER BY p.created_at,p.pack_id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing derivative pack retirements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]packstore.PackUsage, 0, limit)
	for rows.Next() {
		var usage packstore.PackUsage
		var created string
		if err := rows.Scan(&usage.PackID, &usage.EntryCount, &usage.StoredBytes, &created,
			&usage.LiveEntries, &usage.LiveStoredBytes, &usage.LiveRawBytes,
			&usage.MaxLiveStoredLen, &usage.MaxLiveRawLen); err != nil {
			return nil, fmt.Errorf("scanning derivative pack retirement: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, fmt.Errorf("parsing derivative pack %s creation time: %w",
				usage.PackID, err)
		}
		usage.CreatedAt = createdAt
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing derivative pack retirements: %w", err)
	}
	return result, nil
}

// PruneDerivativePackRetirementMappings removes only dead entries from packs
// already named by durable exact-erasure receipts.
func (s *Store) PruneDerivativePackRetirementMappings(
	ctx context.Context, packIDs []string,
) (int64, error) {
	if len(packIDs) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(packIDs)+1)
	args = append(args, s.primaryStoreID)
	for _, packID := range packIDs {
		if packID == "" {
			return 0, errors.New("derivative pack retirement ID is required")
		}
		args = append(args, packID)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM blob_pack_entries
		WHERE store_id=? AND pack_id IN (`+placeholders(len(packIDs))+`)
		  AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash=blob_pack_entries.blob_hash)`, args...)
	if err != nil {
		return 0, fmt.Errorf("pruning derivative pack retirement mappings: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned derivative pack retirement mappings: %w", err)
	}
	return removed, nil
}

// PendingGCLooseRetirements lists a bounded stable page of cleanup authority.
func (s *Store) PendingGCLooseRetirements(
	ctx context.Context, limit int,
) ([]GCLooseRetirement, error) {
	if limit <= 0 {
		return nil, errors.New("GC loose retirement limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT store_id,blob_hash,loose_encoding
		FROM gc_loose_retirements
		ORDER BY store_id,blob_hash,loose_encoding LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing GC loose retirements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]GCLooseRetirement, 0, limit)
	for rows.Next() {
		var item GCLooseRetirement
		var hash string
		var encoding int
		if err := rows.Scan(&item.StoreID, &hash, &encoding); err != nil {
			return nil, fmt.Errorf("scanning GC loose retirement: %w", err)
		}
		parsed, err := packstore.ParseHash(hash)
		if err != nil {
			return nil, fmt.Errorf("parsing GC loose retirement hash: %w", err)
		}
		if encoding != int(packstore.LooseEncodingRaw) &&
			encoding != int(packstore.LooseEncodingZstd) {
			return nil, fmt.Errorf("invalid GC loose retirement encoding %d", encoding)
		}
		item.Hash = parsed
		item.Encoding = packstore.LooseEncoding(encoding)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing GC loose retirements: %w", err)
	}
	return result, nil
}

// PrepareGCLooseRetirement prevents a retry from deleting content that became
// authoritative again. Reauthorized work is consumed without physical removal.
func (s *Store) PrepareGCLooseRetirement(
	ctx context.Context, item GCLooseRetirement,
) (bool, error) {
	retire := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var exists, authorized bool
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM gc_loose_retirements
			 WHERE store_id=? AND blob_hash=? AND loose_encoding=?),
			EXISTS(SELECT 1 FROM blob_locations WHERE store_id=? AND blob_hash=?)`,
			item.StoreID, item.Hash.String(), item.Encoding,
			item.StoreID, item.Hash.String()).Scan(&exists, &authorized); err != nil {
			return fmt.Errorf("checking GC loose retirement: %w", err)
		}
		if !exists {
			return fmt.Errorf("GC loose retirement no longer exists: %w", ErrNotFound)
		}
		if authorized {
			return completeGCLooseRetirementTx(ctx, tx, item)
		}
		retire = true
		return nil
	})
	return retire, err
}

// CompleteGCLooseRetirement acknowledges successful or already-missing cleanup.
func (s *Store) CompleteGCLooseRetirement(
	ctx context.Context, item GCLooseRetirement,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return completeGCLooseRetirementTx(ctx, tx, item)
	})
}

func completeGCLooseRetirementTx(
	ctx context.Context, tx *sql.Tx, item GCLooseRetirement,
) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM gc_loose_retirements
		WHERE store_id=? AND blob_hash=? AND loose_encoding=?`,
		item.StoreID, item.Hash.String(), item.Encoding)
	if err != nil {
		return fmt.Errorf("completing GC loose retirement: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading GC loose retirement completion: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("GC loose retirement no longer exists: %w", ErrNotFound)
	}
	return nil
}

// AllBlobs lists every recorded blob, hash-ordered.
func (s *Store) AllBlobs(ctx context.Context) ([]BlobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash, size FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("listing blobs: %w", err)
	}
	return scanBlobInfos(rows, "listing blobs")
}

// AllBlobHashes lists every recorded blob identity without reading ancillary
// metadata. Integrity verification uses this after separately validating the
// metadata stream, so one malformed scalar does not suppress the useful report.
func (s *Store) AllBlobHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("listing blob hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scanning blob hash: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blob hashes: %w", err)
	}
	return hashes, nil
}

// PackedBlobStoredBytes returns the physical stored length of every cataloged
// packed blob. GC uses it to distinguish bytes unlinked immediately from dead
// immutable-pack space that requires a later repack.
func (s *Store) PackedBlobStoredBytes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT blob_hash, stored_len FROM blob_pack_entries
		WHERE store_id = ?`, s.primaryStoreID)
	if err != nil {
		return nil, fmt.Errorf("listing packed blob sizes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]int64)
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, fmt.Errorf("scanning packed blob size: %w", err)
		}
		result[hash] = size
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing packed blob sizes: %w", err)
	}
	return result, nil
}

// PackedBlobStoredByte reports one blob's immutable-pack payload length.
func (s *Store) PackedBlobStoredByte(ctx context.Context, hash string) (int64, bool, error) {
	var size int64
	err := s.db.QueryRowContext(ctx,
		`SELECT stored_len FROM blob_pack_entries
		 WHERE blob_hash = ? AND store_id = ?`,
		hash, s.primaryStoreID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading packed blob size for %s: %w", hash, err)
	}
	return size, true, nil
}
