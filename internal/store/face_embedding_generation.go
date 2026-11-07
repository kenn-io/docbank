package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document"
)

func FaceEmbeddingPolicyFingerprint() string { return LengthPrefixedSHA256("face/grid/v1") }
func FaceEmbeddingChecksum(preview string, face PhotoFace) string {
	box, _ := json.Marshal(face.Box)
	return LengthPrefixedSHA256(preview, face.FaceID, string(box))
}
func FaceEmbeddingGenerationID(g EmbeddingInputGenerationRecord, binding string) string {
	parts := []string{"docbank/face-input-generation/v1", g.SourceVersionID, g.ProcessingProfileFingerprint, binding, g.EvidenceFingerprint, g.GenerationChecksum}
	for _, input := range g.Inputs {
		parts = append(parts, input.ID, input.RenderedChecksum)
	}
	return LengthPrefixedSHA256(parts...)
}
func validateFaceEmbeddingGeneration(g EmbeddingInputGenerationRecord, binding string) error {
	if g.ChunkPolicyFingerprint != FaceEmbeddingPolicyFingerprint() || g.ID != FaceEmbeddingGenerationID(g, binding) || g.AttachmentID != "" || g.GenerationBlobHash != "" || g.GenerationEncodedSize != 0 || len(g.Inputs) == 0 {
		return errors.New("invalid face generation identity")
	}
	for i, input := range g.Inputs {
		if validateUUIDv4(input.ID) != nil || i > 0 && g.Inputs[i-1].ID >= input.ID {
			return errors.New("face inputs must have sorted unique face IDs")
		}
	}
	return nil
}
func validateFaceEmbeddingGenerationTx(ctx context.Context, tx metadataQuerier, g EmbeddingInputGenerationRecord, binding string) error {
	if err := validateFaceEmbeddingGeneration(g, binding); err != nil {
		return err
	}
	return validateFaceEmbeddingPreviewTx(ctx, tx, g)
}
func validateFaceEmbeddingPreviewTx(ctx context.Context, tx metadataQuerier, g EmbeddingInputGenerationRecord) error {
	if err := liveFaceEmbeddingSource(ctx, tx, g.SourceVersionID); err != nil {
		return err
	}
	faces, err := photoFacesQuery(ctx, tx, g.SourceVersionID)
	if err != nil {
		return err
	}
	quality, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return err
	}
	preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, g.SourceVersionID, quality.GridRecipe)
	if err != nil {
		return err
	}
	if !faceEmbeddingPreviewCurrent(g, faces, preview) {
		return ErrEmbeddingJobFenced
	}
	return nil
}

func faceEmbeddingPreviewCurrent(g EmbeddingInputGenerationRecord, faces []PhotoFace, preview VisualPreviewGeneration) bool {
	if len(faces) != len(g.Inputs) || preview.Checksum != g.EvidenceFingerprint || preview.Preview.State != document.VisualPreviewReady {
		return false
	}
	for i, face := range faces {
		if g.Inputs[i].ID != face.FaceID || g.Inputs[i].RenderedChecksum != FaceEmbeddingChecksum(g.GenerationChecksum, face) {
			return false
		}
	}
	return true
}

func liveFaceEmbeddingSource(ctx context.Context, tx metadataQuerier, version string) error {
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=$1 AND n.current_version_id=v.version_id AND a.kind='photo' AND f.role IN ('image','raw') AND `+liveIncludedDisplayPredicate+`)`, version).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrNotFound
	}
	return nil
}

// FaceEmbeddingFaces returns current photo boxes, including Hidden photos, for local inference.
func (s *Store) FaceEmbeddingFaces(ctx context.Context, version string) ([]PhotoFace, error) {
	var faces []PhotoFace
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		if err := liveFaceEmbeddingSource(ctx, tx, version); err != nil {
			return err
		}
		var err error
		faces, err = photoFacesQuery(ctx, tx, version)
		return err
	})
	return faces, err
}

func faceEmbeddingMissingTx(ctx context.Context, tx *sql.Tx, version, profile string, binding document.EmbeddingBindingV1, recipe string) (bool, error) {
	var faces bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces WHERE content_version_id=$1)`, version).Scan(&faces); err != nil || !faces {
		return false, err
	}
	preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, version, recipe)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if preview.Preview.State != document.VisualPreviewReady {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT generation_id FROM embedding_jobs WHERE content_version_id=$1 AND profile_fingerprint=$2 AND binding_id=$3 AND input_kind='face'`, version, profile, binding.Name)
	if err != nil {
		return false, err
	}
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, id := range ids {
		g, err := loadInputGenerationTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
		if validateFaceEmbeddingGenerationTx(ctx, tx, g, binding.Name) == nil {
			return false, nil
		}
	}
	suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx, EmbeddingHeadKey{version, binding.Name, binding.InputKind}, profile)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return err == nil && !suppression.active, err
}
