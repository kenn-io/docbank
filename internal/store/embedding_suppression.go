package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// Clear failures and capture suppression for every affected binding before
// deleting attachments or sets. Work admitted under a registered profile may
// still be running without a set row. A scoped rendition purge fences bindings
// only when it selects their current rendition; obsolete attachments already
// fail the current-attachment check and must not suppress newer work.
func (s *Store) prepareEmbeddingPurgeTx(
	ctx context.Context, tx *sql.Tx, request PurgeRequest, asOf string,
) (_ []derivativePurgeSuppression, retErr error) {
	if !request.All && len(request.ContentVersionIDs)+len(request.AttachmentIDs)+len(request.BuildIDs) == 0 {
		return nil, nil
	}
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	args := []any{request.All}
	for _, ids := range [][]string{request.ContentVersionIDs, request.AttachmentIDs, request.BuildIDs} {
		encoded, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		args = append(args, string(encoded))
	}
	rows, err := tx.QueryContext(ctx, `WITH scopes AS (
		SELECT cv.version_id,p.profile_fingerprint,1 AS all_bindings
		FROM content_versions cv CROSS JOIN processing_profiles p
		WHERE $1 OR cv.version_id IN (SELECT value FROM `+d.arrayValues("$2")+`)
		UNION ALL
		SELECT a.content_version_id,a.profile_fingerprint,0
		FROM rendition_attachments a
		JOIN rendition_heads h ON h.content_version_id=a.content_version_id
		  AND h.profile_fingerprint=a.profile_fingerprint AND h.attachment_id=a.attachment_id
		WHERE a.attachment_id IN (SELECT value FROM `+d.arrayValues("$3")+`)
		   OR a.build_id IN (SELECT value FROM `+d.arrayValues("$4")+`)
	)
	SELECT cv.version_id,cv.blob_hash,p.profile_fingerprint,p.canonical_profile,MAX(s.all_bindings)
	FROM scopes s JOIN content_versions cv ON cv.version_id=s.version_id
	JOIN processing_profiles p ON p.profile_fingerprint=s.profile_fingerprint
	GROUP BY p.profile_fingerprint,cv.version_id,cv.blob_hash,p.canonical_profile
	ORDER BY p.profile_fingerprint,cv.version_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing embedding purge scopes: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	type scope struct {
		versionID, source, fingerprint string
		allBindings                    bool
	}
	var scopes []scope
	profiles := make(map[string]string)
	for rows.Next() {
		var value scope
		var canonical string
		if err := rows.Scan(&value.versionID, &value.source, &value.fingerprint, &canonical, &value.allBindings); err != nil {
			return nil, fmt.Errorf("reading embedding purge scope: %w", err)
		}
		profiles[value.fingerprint] = canonical
		scopes = append(scopes, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing embedding purge scopes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing embedding purge scopes: %w", err)
	}
	var result []derivativePurgeSuppression
	var lastProfile string
	var profile document.ProcessingProfileV1
	for _, value := range scopes {
		versionID, source, fingerprint, allBindings := value.versionID, value.source, value.fingerprint, value.allBindings
		if fingerprint != lastProfile {
			profile = document.ProcessingProfileV1{}
			if err := json.Unmarshal([]byte(profiles[fingerprint]), &profile); err != nil {
				return nil, fmt.Errorf("decoding embedding purge profile: %w", err)
			}
			lastProfile = fingerprint
		}
		for _, binding := range profile.Embeddings {
			if !allBindings && binding.InputKind != document.EmbeddingInputRenditionChunk {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_failures
				WHERE content_version_id=$1 AND profile_fingerprint=$2 AND binding_id=$3 AND input_kind=$4`,
				versionID, fingerprint, binding.Name, binding.InputKind); err != nil {
				return nil, fmt.Errorf("clearing purged embedding failures: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots WHERE root_kind=$1 AND root_id IN (
                SELECT job_id FROM embedding_jobs WHERE content_version_id=$2 AND profile_fingerprint=$3 AND binding_id=$4 AND input_kind=$5)`,
				RenditionRootWorkerLease, versionID, fingerprint, binding.Name, binding.InputKind); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_jobs WHERE content_version_id=$1 AND profile_fingerprint=$2 AND binding_id=$3 AND input_kind=$4`,
				versionID, fingerprint, binding.Name, binding.InputKind); err != nil {
				return nil, err
			}
			result = append(result, derivativePurgeSuppression{
				sourceSHA256: source, profileFingerprint: derivativeBuildSuppressionProfile,
				buildID:  embeddingPurgeScope(EmbeddingHeadKey{versionID, binding.Name, binding.InputKind}, fingerprint),
				purgedAt: asOf, active: true,
			})
		}
	}
	return result, nil
}

// EmbeddingRebuildAuthorization permits one exact replacement set after an
// explicit purge. Worker staging and publication never grant this authority.
type EmbeddingRebuildAuthorization struct {
	Key                          EmbeddingHeadKey
	ProcessingProfileFingerprint string
	SetID                        string
	AuthorizedAt                 string
}

func (s *Store) AuthorizeEmbeddingRebuild(ctx context.Context, authorization EmbeddingRebuildAuthorization) error {
	if err := validateEmbeddingHeadKey(authorization.Key); err != nil {
		return err
	}
	if err := validateCatalogSHA256(authorization.SetID, "embedding rebuild set ID"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(authorization.ProcessingProfileFingerprint, "embedding rebuild profile"); err != nil {
		return err
	}
	if err := validateMetadataTime("embedding rebuild authorized_at", authorization.AuthorizedAt); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var source string
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=$1`,
			authorization.Key.ContentVersionID).Scan(&source); err != nil {
			return fmt.Errorf("reading embedding rebuild source: %w", err)
		}
		return s.authorizeDerivativeRebuildTx(ctx, tx, DerivativeRebuildAuthorization{
			SourceSHA256:       source,
			PurgedBuildID:      embeddingPurgeScope(authorization.Key, authorization.ProcessingProfileFingerprint),
			SupersedingBuildID: authorization.SetID, AuthorizedAt: authorization.AuthorizedAt,
		}, derivativeBuildSuppressionProfile)
	})
}

// The shared suppression ledger stores an opaque derivative identity in its
// build_id column. A domain-separated binding scope fences every set for this
// version/profile/binding/kind, even when a retry chooses a different set ID.
// Reusing the ledger also preserves its export, restore, and audit authority.
func embeddingPurgeScope(key EmbeddingHeadKey, profileFingerprint string) string {
	return hashCatalogText("docbank:embedding-purge-scope:v1\x00" + key.ContentVersionID + "\x00" +
		profileFingerprint + "\x00" + key.BindingID + "\x00" + string(key.InputKind))
}

func requireEmbeddingPurgeAuthorityTx(ctx context.Context, tx *sql.Tx, set EmbeddingSetRecord) error {
	suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx,
		EmbeddingHeadKey{set.ContentVersionID, set.BindingID, set.InputKind},
		set.ProcessingProfileFingerprint)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking embedding purge suppression: %w", err)
	}
	if suppression.active || suppression.supersedingBuildID != set.ID {
		return fmt.Errorf("embedding binding has a purge suppression without authorization for this set: %w", ErrEmbeddingAuthorityStale)
	}
	return nil
}

func loadEmbeddingPurgeSuppressionTx(
	ctx context.Context, tx *sql.Tx, key EmbeddingHeadKey, profileFingerprint string,
) (derivativePurgeSuppression, error) {
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=$1`,
		key.ContentVersionID).Scan(&source); err != nil {
		return derivativePurgeSuppression{}, fmt.Errorf("reading embedding purge source: %w", err)
	}
	return loadDerivativePurgeSuppressionTx(ctx, tx, source, derivativeBuildSuppressionProfile,
		embeddingPurgeScope(key, profileFingerprint))
}
