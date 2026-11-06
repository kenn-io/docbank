package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

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
 WHERE v.version_id>$1 AND `+liveIncludedPhotoDisplayPredicate("v")+`
 AND EXISTS (SELECT 1 FROM visual_preview_generations g WHERE g.content_version_id=v.version_id
  AND g.recipe_fingerprint=$2 AND g.state='ready')
 AND NOT EXISTS (SELECT 1 FROM photo_quality_signals q WHERE q.content_version_id=v.version_id
  AND q.evaluator_fingerprint=$3 AND q.face_inputs=`+s.photoFaceInputsSQL("v.version_id")+`)
 ORDER BY v.version_id LIMIT $4`, after, fingerprints.GridRecipe, fingerprints.Evaluator, limit)
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
	ctx context.Context, target PhotoVisualPreviewTarget, signals document.PhotoQualitySignals, inputs string,
) error {
	if err := document.ValidatePhotoQualitySignals(signals); err != nil {
		return err
	}
	return s.publishPhotoQuality(ctx, target, photoQualityStateReady, []any{
		signals.Focus, signals.Blur, signals.Brightness, signals.ColorRed,
		signals.ColorGreen, signals.ColorBlue, signals.Framing, signals.Aesthetics,
	}, signals.FaceReports, inputs)
}

// PublishPhotoQualityUnavailable records a terminal result for verified
// preview bytes that the evaluator cannot measure.
func (s *Store) PublishPhotoQualityUnavailable(ctx context.Context, target PhotoVisualPreviewTarget, inputs string, reports ...document.PhotoFaceReport) error {
	return s.publishPhotoQuality(ctx, target, photoQualityStateUnavailable, make([]any, 8), reports, inputs)
}

func (s *Store) publishPhotoQuality(
	ctx context.Context, target PhotoVisualPreviewTarget, state string, scores []any, reports []document.PhotoFaceReport, inputs string,
) error {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(reports)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		args := append([]any{fingerprints.Evaluator, state}, scores...)
		args = append(args, string(encoded), inputs, target.VersionID, target.SourceSHA256, target.Size, target.MediaType, inputs)
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_quality_signals
 WHERE content_version_id=$1 AND evaluator_fingerprint!=$2`, target.VersionID, fingerprints.Evaluator); err != nil {
			return fmt.Errorf("replacing stale photo quality signals: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO photo_quality_signals(
 content_version_id,evaluator_fingerprint,state,`+photoQualityColumns+`,face_reports_json,face_inputs)
 SELECT v.version_id,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12 FROM content_versions v
 WHERE v.version_id=$13 AND v.blob_hash=$14 AND v.size=$15 AND COALESCE(v.mime_type,'')=$16
 AND `+liveIncludedPhotoDisplayPredicate("v")+` AND `+s.photoFaceInputsSQL("v.version_id")+`=$17
 ON CONFLICT(content_version_id,evaluator_fingerprint) DO UPDATE SET
 state=excluded.state,focus=excluded.focus,blur=excluded.blur,brightness=excluded.brightness,
 color_red=excluded.color_red,color_green=excluded.color_green,color_blue=excluded.color_blue,
 framing=excluded.framing,aesthetics=excluded.aesthetics,face_reports_json=excluded.face_reports_json,face_inputs=excluded.face_inputs
 WHERE photo_quality_signals.face_inputs<>excluded.face_inputs`, args...)
		return err
	})
}

// photoQualityResults holds measured signals and terminal unavailable versions.
type photoQualityResults struct {
	staleFaces  map[string]bool
	signals     map[string]document.PhotoQualitySignals
	unavailable map[string]bool
}

func (s *Store) photoQualityForVersions(
	ctx context.Context, q metadataQuerier, versions []string,
) (photoQualityResults, error) {
	result := photoQualityResults{
		staleFaces:  make(map[string]bool),
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
	rows, err := q.QueryContext(ctx, `SELECT content_version_id,state,`+photoQualityColumns+`,face_reports_json,face_inputs,`+s.photoFaceInputsSQL("photo_quality_signals.content_version_id")+`
 FROM photo_quality_signals
 WHERE evaluator_fingerprint=$1 AND content_version_id IN (`+numberedPlaceholders(len(versions), 1)+`)`, args...)
	if err != nil {
		return photoQualityResults{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state, inputs, current string
		var reports []byte
		var v [8]sql.NullFloat64
		if err := rows.Scan(&id, &state, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &reports, &inputs, &current); err != nil {
			return photoQualityResults{}, err
		}
		signals, unavailable, err := decodePhotoQualityRow(state, v)
		if err != nil {
			return photoQualityResults{}, fmt.Errorf("reading photo quality for %s: %w", id, err)
		}
		if unavailable {
			result.unavailable[id] = true
		}
		result.staleFaces[id] = inputs != current
		if inputs == current {
			if err := json.Unmarshal(reports, &signals.FaceReports); err != nil {
				return result, err
			}
		}
		for i := range signals.FaceReports {
			signals.FaceReports[i].Grade = signals.FaceReports[i].ReportGrade()
		}
		if !unavailable || len(signals.FaceReports) > 0 {
			result.signals[id] = signals
		}
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

// The source signature is captured with faces and rechecked at publication.
// photoFaceInputsSQL identifies the current face inputs to a disposable quality
// cache. Its encoding is backend-local; quality rows never enter metadata transfer.
func (s *Store) photoFaceInputsSQL(version string) string {
	selection := ` FROM photo_faces WHERE content_version_id=` + version + ` AND NOT EXISTS(SELECT 1 FROM person_match_candidates c JOIN person_candidate_faces e ON e.candidate_id=c.candidate_id WHERE e.face_id=photo_faces.face_id AND ` + rejectedFaceMatchPredicate + ` AND COALESCE(c.suggested_person_id,'')='')`
	if s.postgresSchema != "" {
		return `(SELECT COALESCE(string_agg(json_build_array(face_id,x,y,width,height,facing,eyes_open,smile)::text,',' ORDER BY face_id),'')` + selection + `)`
	}
	return `(SELECT COALESCE(group_concat(input,','),'') FROM (SELECT json_array(face_id,x,y,width,height,facing,eyes_open,smile) input` + selection + ` ORDER BY face_id))`
}

func (s *Store) PhotoQualityFaces(ctx context.Context, version string) ([]PhotoFace, string, error) {
	var faces []PhotoFace
	var inputs string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		faces, err = s.photoQualityFacesTx(ctx, tx, version)
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT `+s.photoFaceInputsSQL("$1"), version).Scan(&inputs)
	})
	return faces, inputs, err
}

func (s *Store) photoQualityFacesTx(ctx context.Context, tx *sql.Tx, version string) ([]PhotoFace, error) {
	faces, err := photoFacesQuery(ctx, tx, version)
	if err == nil {
		err = s.readFaceDecisionsTx(ctx, tx, faces)
	}
	return slices.DeleteFunc(faces, func(f PhotoFace) bool { return f.Dismissed }), err
}
