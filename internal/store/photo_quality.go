package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/document"
)

const photoQualityColumns = `focus,blur,brightness,color_red,color_green,color_blue,framing,aesthetics`

func (s *Store) MissingPhotoQualityTargetsAfter(ctx context.Context, after string, limit int) ([]PhotoVisualPreviewTarget, error) {
	if limit <= 0 {
		return nil, errors.New("photo quality target limit must be positive")
	}
	recipe, _ := document.BuiltInVisualPreviewRecipe("grid")
	_, fingerprint, _ := document.MarshalVisualPreviewRecipeV1(recipe)
	rows, err := s.db.QueryContext(ctx, `SELECT v.version_id,v.blob_hash,v.size,COALESCE(v.mime_type,'') FROM content_versions v
 WHERE v.version_id>? AND `+liveIncludedPhotoDisplayPredicate+`
 AND EXISTS (SELECT 1 FROM visual_preview_generations g WHERE g.content_version_id=v.version_id AND g.recipe_fingerprint=? AND g.state='ready')
 AND NOT EXISTS (SELECT 1 FROM photo_quality_signals q WHERE q.content_version_id=v.version_id AND q.evaluator_fingerprint=?)
 ORDER BY v.version_id LIMIT ?`, after, fingerprint, document.PhotoQualityEvaluatorFingerprint(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var targets []PhotoVisualPreviewTarget
	for rows.Next() {
		var t PhotoVisualPreviewTarget
		if err := rows.Scan(&t.VersionID, &t.SourceSHA256, &t.Size, &t.MediaType); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// PublishPhotoQualitySignals rechecks current display ownership in the write transaction.
func (s *Store) PublishPhotoQualitySignals(ctx context.Context, target PhotoVisualPreviewTarget, signals document.PhotoQualitySignals) error {
	if err := document.ValidatePhotoQualitySignals(signals); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_quality_signals(content_version_id,evaluator_fingerprint,`+photoQualityColumns+`)
 SELECT v.version_id,?,?,?,?,?,?,?,?,? FROM content_versions v WHERE v.version_id=? AND v.blob_hash=? AND v.size=? AND COALESCE(v.mime_type,'')=? AND `+liveIncludedPhotoDisplayPredicate+`
 ON CONFLICT(content_version_id,evaluator_fingerprint) DO NOTHING`, document.PhotoQualityEvaluatorFingerprint(), signals.Focus, signals.Blur, signals.Brightness, signals.ColorRed, signals.ColorGreen, signals.ColorBlue, signals.Framing, signals.Aesthetics, target.VersionID, target.SourceSHA256, target.Size, target.MediaType)
		return err
	})
}

func photoQualityForVersions(ctx context.Context, q metadataQuerier, versions []string) (map[string]document.PhotoQualitySignals, error) {
	result := make(map[string]document.PhotoQualitySignals)
	if len(versions) == 0 {
		return result, nil
	}
	args := []any{document.PhotoQualityEvaluatorFingerprint()}
	for _, v := range versions {
		args = append(args, v)
	}
	rows, err := q.QueryContext(ctx, `SELECT content_version_id,`+photoQualityColumns+` FROM photo_quality_signals WHERE evaluator_fingerprint=? AND content_version_id IN (`+placeholders(len(versions))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var s document.PhotoQualitySignals
		if err := rows.Scan(&id, &s.Focus, &s.Blur, &s.Brightness, &s.ColorRed, &s.ColorGreen, &s.ColorBlue, &s.Framing, &s.Aesthetics); err != nil {
			return nil, err
		}
		if err := document.ValidatePhotoQualitySignals(s); err != nil {
			return nil, err
		}
		result[id] = s
	}
	return result, rows.Err()
}
