package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// VisualPreviewGeneration is one immutable result for an exact content
// version and complete recipe identity.
type VisualPreviewGeneration struct {
	GenerationID      string
	VaultID           string
	ContentVersionID  string
	RecipeFingerprint string
	CanonicalResult   []byte
	Checksum          string
	Preview           document.VisualPreviewV1
	CreatedAt         string
}

// VisualPreviewView is the active preview result joined to its exact source
// version and publication time.
type VisualPreviewView struct {
	Version     ContentVersion
	Generation  VisualPreviewGeneration
	PublishedAt string
}

func visualPreviewGenerationID(versionID, recipeFingerprint, checksum string) string {
	h := sha256.New()
	for _, value := range []string{versionID, recipeFingerprint, checksum} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PublishVisualPreview validates and atomically publishes one exact-version
// result. Ready results also commit the already-durable output blob receipt.
// The active head advances only when recording a new generation or retrying
// the current head. Retrying the identical publication is idempotent.
func (s *Store) PublishVisualPreview(
	ctx context.Context, versionID string, canonical []byte, physical *BlobPhysical,
) (VisualPreviewGeneration, error) {
	if err := validateUUIDv4(versionID); err != nil {
		return VisualPreviewGeneration{}, fmt.Errorf("content version %q: %w", versionID, ErrNotFound)
	}
	preview, checksum, err := document.DecodeVisualPreviewV1(canonical)
	if err != nil {
		return VisualPreviewGeneration{}, fmt.Errorf("validating visual preview: %w", err)
	}
	_, recipeFingerprint, err := document.MarshalVisualPreviewRecipeV1(preview.Recipe)
	if err != nil {
		return VisualPreviewGeneration{}, fmt.Errorf("fingerprinting visual preview recipe: %w", err)
	}
	if preview.State == document.VisualPreviewReady && physical == nil {
		return VisualPreviewGeneration{}, errors.New("ready visual preview requires physical blob authority")
	}
	if preview.State != document.VisualPreviewReady && physical != nil {
		return VisualPreviewGeneration{}, errors.New("non-ready visual preview must not carry physical blob authority")
	}
	generation := VisualPreviewGeneration{
		GenerationID: visualPreviewGenerationID(versionID, recipeFingerprint, checksum),
		VaultID:      s.vaultID, ContentVersionID: versionID, RecipeFingerprint: recipeFingerprint,
		CanonicalResult: append([]byte(nil), canonical...), Checksum: checksum,
		Preview: preview, CreatedAt: nowRFC3339(),
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		version, readErr := scanContentVersion(tx.QueryRowContext(ctx,
			`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, versionID))
		if readErr != nil {
			return fmt.Errorf("reading visual preview source version: %w", readErr)
		}
		if version.BlobHash != preview.SourceSHA256 {
			return errors.New("visual preview source does not match content version")
		}
		if preview.State == document.VisualPreviewReady {
			if err := s.EnsureBlobTx(tx, preview.Output.BlobSHA256, preview.Output.Size, *physical); err != nil {
				return fmt.Errorf("recording visual preview output: %w", err)
			}
		}
		result, execErr := tx.ExecContext(ctx, `INSERT INTO visual_preview_generations(
			generation_id,vault_uid,content_version_id,source_sha256,contract_version,
			recipe_fingerprint,canonical_result,checksum,state,output_blob_hash,output_size,
			output_media_type,output_width,output_height,failure_code,failure_detail,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(content_version_id,recipe_fingerprint) DO NOTHING`,
			generation.GenerationID, generation.VaultID, versionID, preview.SourceSHA256,
			preview.ContractVersion, recipeFingerprint, canonical, checksum, preview.State,
			previewOutputHash(preview), previewOutputSize(preview), previewOutputMediaType(preview),
			previewOutputWidth(preview), previewOutputHeight(preview), previewFailureCode(preview),
			previewFailureDetail(preview), generation.CreatedAt)
		if execErr != nil {
			return fmt.Errorf("recording visual preview generation: %w", execErr)
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("checking visual preview generation insertion: %w", rowsErr)
		}
		stored, readErr := visualPreviewGenerationByRecipeTx(ctx, tx, versionID, recipeFingerprint)
		if readErr != nil {
			return readErr
		}
		if stored.GenerationID != generation.GenerationID || stored.Checksum != checksum ||
			!bytes.Equal(stored.CanonicalResult, canonical) {
			return errors.New("visual preview recipe already has a different result")
		}
		generation = stored
		_, execErr = tx.ExecContext(ctx, `INSERT INTO visual_preview_heads(
			content_version_id,generation_id,published_at
		) VALUES(?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET
			generation_id=excluded.generation_id,published_at=excluded.published_at
			WHERE ? != 0 OR visual_preview_heads.generation_id=excluded.generation_id`,
			versionID, generation.GenerationID, nowRFC3339(), inserted)
		return execErr
	})
	return generation, err
}

// ContentVersionVisualPreview returns the active result for one exact content
// version, including deterministic unsupported and failed outcomes.
func (s *Store) ContentVersionVisualPreview(
	ctx context.Context, versionID string,
) (VisualPreviewView, error) {
	if err := validateUUIDv4(versionID); err != nil {
		return VisualPreviewView{}, fmt.Errorf("content version %q: %w", versionID, ErrNotFound)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return VisualPreviewView{}, fmt.Errorf("starting visual preview snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, versionID))
	if err != nil {
		return VisualPreviewView{}, fmt.Errorf("content version %q: %w", versionID, err)
	}
	var publishedAt string
	var recipeFingerprint string
	err = tx.QueryRowContext(ctx, `SELECT g.recipe_fingerprint,h.published_at
		FROM visual_preview_heads h JOIN visual_preview_generations g
		ON g.generation_id=h.generation_id WHERE h.content_version_id=?`, versionID).
		Scan(&recipeFingerprint, &publishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return VisualPreviewView{}, ErrNotFound
	}
	if err != nil {
		return VisualPreviewView{}, fmt.Errorf("reading visual preview head: %w", err)
	}
	generation, err := visualPreviewGenerationByRecipeTx(ctx, tx, versionID, recipeFingerprint)
	if err != nil {
		return VisualPreviewView{}, err
	}
	if generation.VaultID != s.vaultID {
		return VisualPreviewView{}, errors.New("stored visual preview belongs to another vault")
	}
	if generation.Preview.SourceSHA256 != version.BlobHash {
		return VisualPreviewView{}, errors.New("stored visual preview source does not match content version")
	}
	if err := tx.Commit(); err != nil {
		return VisualPreviewView{}, fmt.Errorf("closing visual preview snapshot: %w", err)
	}
	return VisualPreviewView{Version: version, Generation: generation, PublishedAt: publishedAt}, nil
}

func visualPreviewGenerationByRecipeTx(
	ctx context.Context, q metadataQuerier, versionID, recipeFingerprint string,
) (VisualPreviewGeneration, error) {
	var generation VisualPreviewGeneration
	var sourceSHA256, contractVersion, state string
	var outputHash, outputMediaType, failureCode, failureDetail sql.NullString
	var outputSize, outputWidth, outputHeight sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT generation_id,vault_uid,content_version_id,
		recipe_fingerprint,canonical_result,checksum,created_at,source_sha256,contract_version,
		state,output_blob_hash,output_size,output_media_type,output_width,output_height,
		failure_code,failure_detail
		FROM visual_preview_generations WHERE content_version_id=? AND recipe_fingerprint=?`,
		versionID, recipeFingerprint).Scan(&generation.GenerationID, &generation.VaultID,
		&generation.ContentVersionID, &generation.RecipeFingerprint, &generation.CanonicalResult,
		&generation.Checksum, &generation.CreatedAt, &sourceSHA256, &contractVersion, &state,
		&outputHash, &outputSize, &outputMediaType, &outputWidth, &outputHeight,
		&failureCode, &failureDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return VisualPreviewGeneration{}, ErrNotFound
	}
	if err != nil {
		return VisualPreviewGeneration{}, fmt.Errorf("reading visual preview generation: %w", err)
	}
	preview, checksum, err := document.DecodeVisualPreviewV1(generation.CanonicalResult)
	if err != nil || checksum != generation.Checksum ||
		visualPreviewGenerationID(versionID, recipeFingerprint, checksum) != generation.GenerationID {
		return VisualPreviewGeneration{}, errors.New("stored visual preview failed canonical identity validation")
	}
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(preview.Recipe)
	if err != nil || fingerprint != recipeFingerprint {
		return VisualPreviewGeneration{}, errors.New("stored visual preview recipe fingerprint is invalid")
	}
	if err := validateVisualPreviewStorage(preview, sourceSHA256, contractVersion, state,
		outputHash, outputSize, outputMediaType, outputWidth, outputHeight,
		failureCode, failureDetail); err != nil {
		return VisualPreviewGeneration{}, err
	}
	generation.Preview = preview
	return generation, nil
}

func validateVisualPreviewStorage(
	preview document.VisualPreviewV1, sourceSHA256, contractVersion, state string,
	outputHash sql.NullString, outputSize sql.NullInt64, outputMediaType sql.NullString,
	outputWidth, outputHeight sql.NullInt64, failureCode, failureDetail sql.NullString,
) error {
	if sourceSHA256 != preview.SourceSHA256 || contractVersion != preview.ContractVersion ||
		state != string(preview.State) {
		return errors.New("stored visual preview columns do not match canonical result")
	}
	if preview.Output != nil {
		if !outputHash.Valid || outputHash.String != preview.Output.BlobSHA256 ||
			!outputSize.Valid || outputSize.Int64 != preview.Output.Size ||
			!outputMediaType.Valid || outputMediaType.String != preview.Output.MediaType ||
			!outputWidth.Valid || outputWidth.Int64 != int64(preview.Output.Width) ||
			!outputHeight.Valid || outputHeight.Int64 != int64(preview.Output.Height) ||
			failureCode.Valid || failureDetail.Valid {
			return errors.New("stored visual preview output does not match canonical result")
		}
		return nil
	}
	if outputHash.Valid || outputSize.Valid || outputMediaType.Valid || outputWidth.Valid || outputHeight.Valid ||
		!failureCode.Valid || failureCode.String != preview.Failure.Code ||
		!failureDetail.Valid || failureDetail.String != preview.Failure.Detail {
		return errors.New("stored visual preview failure does not match canonical result")
	}
	return nil
}

func previewOutputHash(value document.VisualPreviewV1) any {
	if value.Output == nil {
		return nil
	}
	return value.Output.BlobSHA256
}

func previewOutputSize(value document.VisualPreviewV1) any {
	if value.Output == nil {
		return nil
	}
	return value.Output.Size
}

func previewOutputMediaType(value document.VisualPreviewV1) any {
	if value.Output == nil {
		return nil
	}
	return value.Output.MediaType
}

func previewOutputWidth(value document.VisualPreviewV1) any {
	if value.Output == nil {
		return nil
	}
	return value.Output.Width
}

func previewOutputHeight(value document.VisualPreviewV1) any {
	if value.Output == nil {
		return nil
	}
	return value.Output.Height
}

func previewFailureCode(value document.VisualPreviewV1) any {
	if value.Failure == nil {
		return nil
	}
	return value.Failure.Code
}

func previewFailureDetail(value document.VisualPreviewV1) any {
	if value.Failure == nil {
		return nil
	}
	return value.Failure.Detail
}
