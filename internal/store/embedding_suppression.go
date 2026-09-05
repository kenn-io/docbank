package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

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
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`,
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
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`,
		set.ContentVersionID).Scan(&source); err != nil {
		return fmt.Errorf("reading embedding purge source: %w", err)
	}
	scope := embeddingPurgeScope(EmbeddingHeadKey{set.ContentVersionID, set.BindingID, set.InputKind},
		set.ProcessingProfileFingerprint)
	suppression, err := loadDerivativePurgeSuppressionTx(ctx, tx, source, derivativeBuildSuppressionProfile, scope)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking embedding purge suppression: %w", err)
	}
	if suppression.active || suppression.supersedingBuildID != set.ID {
		return errors.New("embedding binding has a purge suppression without authorization for this set")
	}
	return nil
}
