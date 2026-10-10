package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

const (
	photoQualityColumns          = `focus,blur,brightness,color_red,color_green,color_blue,framing,aesthetics`
	photoQualityStateReady       = "ready"
	photoQualityStateUnavailable = "unavailable"
)

func (s *Store) MissingPhotoQualityTargetsAfter(
	ctx context.Context, after string, limit int,
) ([]PhotoVisualPreviewTarget, error) {
	if limit <= 0 {
		return nil, errors.New("photo quality target limit must be positive")
	}
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT v.version_id,v.blob_hash,v.size,COALESCE(v.mime_type,'')
 FROM content_versions v
 WHERE v.version_id>? AND `+liveIncludedPhotoDisplayPredicate("v")+`
 AND EXISTS (SELECT 1 FROM visual_preview_generations g WHERE g.content_version_id=v.version_id
  AND g.recipe_fingerprint=? AND g.state='ready')
 AND NOT EXISTS (SELECT 1 FROM photo_quality_signals q WHERE q.content_version_id=v.version_id
  AND q.evaluator_fingerprint=?)
 ORDER BY v.version_id LIMIT ?`, after, fingerprints.GridRecipe, fingerprints.Evaluator, limit)
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

// PublishPhotoQualitySignals rechecks current display ownership in the write
// transaction and replaces the version's measurements from other evaluators.
func (s *Store) PublishPhotoQualitySignals(
	ctx context.Context, target PhotoVisualPreviewTarget, signals document.PhotoQualitySignals,
) error {
	if err := document.ValidatePhotoQualitySignals(signals); err != nil {
		return err
	}
	return s.publishPhotoQuality(ctx, target, photoQualityStateReady, []any{
		signals.Focus, signals.Blur, signals.Brightness, signals.ColorRed,
		signals.ColorGreen, signals.ColorBlue, signals.Framing, signals.Aesthetics,
	})
}

// PublishPhotoQualityUnavailable records a terminal result for verified
// preview bytes that the evaluator cannot measure.
func (s *Store) PublishPhotoQualityUnavailable(ctx context.Context, target PhotoVisualPreviewTarget) error {
	return s.publishPhotoQuality(ctx, target, photoQualityStateUnavailable, make([]any, 8))
}

func (s *Store) publishPhotoQuality(
	ctx context.Context, target PhotoVisualPreviewTarget, state string, scores []any,
) error {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return err
	}
	args := append([]any{fingerprints.Evaluator, state}, scores...)
	args = append(args, target.VersionID, target.SourceSHA256, target.Size, target.MediaType)
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_quality_signals
 WHERE content_version_id=? AND evaluator_fingerprint!=?`, target.VersionID, fingerprints.Evaluator); err != nil {
			return fmt.Errorf("replacing stale photo quality signals: %w", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_quality_signals(
 content_version_id,evaluator_fingerprint,state,`+photoQualityColumns+`)
 SELECT v.version_id,?,?,?,?,?,?,?,?,?,? FROM content_versions v
 WHERE v.version_id=? AND v.blob_hash=? AND v.size=? AND COALESCE(v.mime_type,'')=?
 AND `+liveIncludedPhotoDisplayPredicate("v")+`
 ON CONFLICT(content_version_id,evaluator_fingerprint) DO NOTHING`, args...)
		return err
	})
}

// photoQualityResults holds measured signals and terminal unavailable versions.
type photoQualityResults struct {
	signals     map[string]document.PhotoQualitySignals
	unavailable map[string]bool
}

func photoQualityForVersions(
	ctx context.Context, q metadataQuerier, versions []string,
) (photoQualityResults, error) {
	result := photoQualityResults{
		signals:     make(map[string]document.PhotoQualitySignals),
		unavailable: make(map[string]bool),
	}
	if len(versions) == 0 {
		return result, nil
	}
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return photoQualityResults{}, err
	}
	args := []any{fingerprints.Evaluator}
	for _, v := range versions {
		args = append(args, v)
	}
	rows, err := q.QueryContext(ctx, `SELECT content_version_id,state,`+photoQualityColumns+`
 FROM photo_quality_signals
 WHERE evaluator_fingerprint=? AND content_version_id IN (`+placeholders(len(versions))+`)`, args...)
	if err != nil {
		return photoQualityResults{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state string
		var v [8]sql.NullFloat64
		if err := rows.Scan(&id, &state, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
			return photoQualityResults{}, err
		}
		signals, unavailable, err := decodePhotoQualityRow(state, v)
		if err != nil {
			return photoQualityResults{}, fmt.Errorf("reading photo quality for %s: %w", id, err)
		}
		if unavailable {
			result.unavailable[id] = true
			continue
		}
		result.signals[id] = signals
	}
	return result, rows.Err()
}

// decodePhotoQualityRow rejects unknown states and score presence that does
// not match the state.
func decodePhotoQualityRow(
	state string, v [8]sql.NullFloat64,
) (document.PhotoQualitySignals, bool, error) {
	present := 0
	for _, score := range v {
		if score.Valid {
			present++
		}
	}
	switch {
	case state == photoQualityStateUnavailable && present == 0:
		return document.PhotoQualitySignals{}, true, nil
	case state == photoQualityStateReady && present == len(v):
		signals := document.PhotoQualitySignals{
			Focus: v[0].Float64, Blur: v[1].Float64, Brightness: v[2].Float64, ColorRed: v[3].Float64,
			ColorGreen: v[4].Float64, ColorBlue: v[5].Float64, Framing: v[6].Float64, Aesthetics: v[7].Float64,
		}
		return signals, false, document.ValidatePhotoQualitySignals(signals)
	default:
		return document.PhotoQualitySignals{}, false, fmt.Errorf(
			"invalid photo quality state %q with %d of %d scores", state, present, len(v))
	}
}
