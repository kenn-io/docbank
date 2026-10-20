package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	photoSuggestionAccepted  = "accepted"
	photoSuggestionDismissed = "dismissed"
)

var (
	ErrInvalidPhotoDescriptionResult  = errors.New("invalid photo description result")
	ErrInvalidPhotoSuggestionResponse = errors.New("invalid photo suggestion response")
	ErrPhotoSuggestionConflict        = errors.New("photo suggestion field is no longer pending")
)

// PhotoSuggestion is the pending part of one photo's description. Fields the
// owner accepted or dismissed are no longer in it.
type PhotoSuggestion struct {
	AssetID          string                    `json:"asset_id"`
	AssetRevision    int64                     `json:"asset_revision"`
	SourceRevision   int64                     `json:"source_revision"`
	NodeRevision     int64                     `json:"node_revision"`
	AuthoredReceipt  *PhotoAuthoredReceipt     `json:"authored_receipt,omitempty"`
	ID               string                    `json:"suggestion_id"`
	ContentVersionID string                    `json:"content_version_id"`
	Target           string                    `json:"target"`
	Value            document.PhotoDescription `json:"value"`
}

// photoSuggestionSettledSQL is true when no description is wanted for version
// from producer. Automatic work wants one only for a version without rows. An
// explicit start also wants one when every row is another producer's
// dismissal. A row awaiting regeneration after restore wants one from a
// producer not dismissed on it.
func photoSuggestionSettledSQL(version, producer string, explicit bool) string {
	filter := ""
	if explicit {
		filter = ` AND (NOT s.dismissed OR s.producer_fingerprint=` + producer + `)`
	}
	return `(EXISTS(SELECT 1 FROM photo_suggestions s WHERE s.content_version_id=` + version + filter + `)
 AND (NOT EXISTS(SELECT 1 FROM photo_suggestions s WHERE s.content_version_id=` + version + ` AND s.awaiting)
  OR EXISTS(SELECT 1 FROM photo_suggestions s WHERE s.content_version_id=` + version + ` AND s.dismissed AND s.producer_fingerprint=` + producer + `)))`
}

var photoDescriptionEligibility = liveIncludedPhotoDisplayPredicate("v") + ` AND a.hidden_at IS NULL`

func photoDescriptionEligibleTx(ctx context.Context, q metadataQuerier, versionID string) (bool, error) {
	var eligible bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=? AND `+photoDescriptionEligibility+`)`, versionID).Scan(&eligible)
	return eligible, err
}

// PhotoDescriptionEligible reports whether the version is a visible, included display photo.
func (s *Store) PhotoDescriptionEligible(ctx context.Context, versionID string) (bool, error) {
	return photoDescriptionEligibleTx(ctx, s.db, versionID)
}

// photoDescriptionSettled reports whether no description is wanted for the
// version from producer. Explicit applies the explicit-start predicate.
func (s *Store) photoDescriptionSettled(ctx context.Context, versionID, producer string, explicit bool) (bool, error) {
	return photoDescriptionSettledTx(ctx, s.db, versionID, producer, explicit)
}

func photoDescriptionSettledTx(ctx context.Context, q metadataQuerier, versionID, producer string, explicit bool) (bool, error) {
	var settled bool
	err := q.QueryRowContext(ctx, `SELECT `+photoSuggestionSettledSQL("?1", "?2", explicit), versionID, producer).Scan(&settled)
	return settled, err
}

// dropOwnedPhotoFieldsTx clears the caption equal to the authored caption and
// the tags already assigned to the version's photo. It reports false when the
// version has no photo file.
func dropOwnedPhotoFieldsTx(ctx context.Context, q metadataQuerier, version string, description *document.PhotoDescription) (bool, error) {
	var authored string
	if err := q.QueryRowContext(ctx, `SELECT f.caption FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=?`, version).Scan(&authored); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var assigned []string
	if err := queryRows(ctx, q, `SELECT t.name FROM content_versions v JOIN node_tags nt ON nt.node_id=v.node_id JOIN tags t ON t.id=nt.tag_id WHERE v.version_id=?`, []any{version}, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		assigned = append(assigned, name)
		return nil
	}); err != nil {
		return false, err
	}
	if description.Caption == authored {
		description.Caption = ""
	}
	description.Tags = slices.DeleteFunc(description.Tags, func(tag string) bool { return slices.Contains(assigned, tag) })
	return true, nil
}

// publishPhotoSuggestionTx stores a provider's description for a version
// unless the version is settled. The description is already normalized. Fields
// the owner already has or resolved before a restore are left out.
func publishPhotoSuggestionTx(ctx context.Context, tx *sql.Tx, version, producer string, explicit bool, description document.PhotoDescription, at time.Time) error {
	if settled, err := photoDescriptionSettledTx(ctx, tx, version, producer, explicit); err != nil || settled {
		return err
	}
	if ok, err := dropOwnedPhotoFieldsTx(ctx, tx, version, &description); err != nil || !ok {
		return err
	}
	var awaiting int64
	var captionResolved bool
	var resolvedJSON string
	err := tx.QueryRowContext(ctx, `SELECT id,caption_resolved,resolved_tags_json FROM photo_suggestions WHERE content_version_id=? AND awaiting ORDER BY producer_fingerprint=? DESC,id LIMIT 1`, version, producer).Scan(&awaiting, &captionResolved, &resolvedJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var resolved []string
	if resolvedJSON != "" {
		if err := json.Unmarshal([]byte(resolvedJSON), &resolved); err != nil {
			return err
		}
	}
	if captionResolved {
		description.Caption = ""
	}
	description.Tags = slices.DeleteFunc(description.Tags, func(tag string) bool { return slices.Contains(resolved, tag) })
	tags, err := json.Marshal(description.Tags)
	if err != nil {
		return err
	}
	if awaiting != 0 {
		// The restored row now belongs to the producer that regenerated it. An
		// older row of that producer is superseded by the owner's decisions it holds.
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_suggestions WHERE content_version_id=? AND producer_fingerprint=? AND id<>?`, version, producer, awaiting); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE photo_suggestions SET producer_fingerprint=?,caption=?,tags_json=?,visible_text=?,awaiting=0 WHERE id=?`, producer, description.Caption, string(tags), description.VisibleText, awaiting)
		return err
	}
	id, err := newUUIDv4()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,caption,tags_json,visible_text,created_at) VALUES(?,?,?,?,?,?,?)`, id, version, producer, description.Caption, string(tags), description.VisibleText, at.UTC().Format(timestampLayout))
	return err
}

// normalizePhotoDescription gives tags their stored NFC identity and drops duplicates.
func normalizePhotoDescription(description document.PhotoDescription) (document.PhotoDescription, error) {
	normalized := make([]string, 0, len(description.Tags))
	for _, name := range description.Tags {
		name, err := NormalizeTagName(name)
		if err != nil {
			return description, err
		}
		if !slices.Contains(normalized, name) {
			normalized = append(normalized, name)
		}
	}
	description.Tags = normalized
	return description, document.ValidatePhotoDescription(description)
}

// PhotoSuggestionResponse accepts selected pending fields or dismisses the rest.
type PhotoSuggestionResponse struct {
	AssetID        string   `json:"asset_id"`
	AssetRevision  int64    `json:"asset_revision"`
	SourceRevision int64    `json:"source_revision"`
	NodeRevision   int64    `json:"node_revision"`
	Response       string   `json:"response" enum:"accepted,dismissed"`
	Caption        bool     `json:"caption"`
	Tags           []string `json:"tags" maxItems:"64"`
}

type photoSuggestionRow struct {
	PhotoSuggestion

	id              int64
	producer        string
	dismissed       bool
	awaiting        bool
	captionResolved bool
	resolved        []string
	hidden          bool
}

const photoSuggestionRowSQL = `SELECT s.id,s.suggestion_id,s.content_version_id,s.producer_fingerprint,s.caption,s.tags_json,s.visible_text,s.dismissed,s.awaiting,s.caption_resolved,s.resolved_tags_json,
 f.file_id,f.revision,n.revision,a.asset_id,a.revision,a.hidden_at IS NOT NULL
 FROM photo_suggestions s JOIN content_versions v ON v.version_id=s.content_version_id
 JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
 JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id`

func scanPhotoSuggestion(row interface{ Scan(dest ...any) error }) (photoSuggestionRow, error) {
	var value photoSuggestionRow
	var tags, resolved string
	err := row.Scan(&value.id, &value.ID, &value.ContentVersionID, &value.producer, &value.Value.Caption, &tags, &value.Value.VisibleText,
		&value.dismissed, &value.awaiting, &value.captionResolved, &resolved,
		&value.Target, &value.SourceRevision, &value.NodeRevision, &value.AssetID, &value.AssetRevision, &value.hidden)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(tags), &value.Value.Tags); err != nil {
		return value, err
	}
	return value, json.Unmarshal([]byte(resolved), &value.resolved)
}

// RespondPhotoSuggestion accepts the requested pending fields as authored
// values in the same transaction that clears them, or dismisses what remains.
func (s *Store) RespondPhotoSuggestion(ctx context.Context, id string, request PhotoSuggestionResponse) (PhotoSuggestion, error) {
	accepted := request.Response == photoSuggestionAccepted
	if !accepted && request.Response != photoSuggestionDismissed || accepted == (!request.Caption && len(request.Tags) == 0) {
		return PhotoSuggestion{}, ErrInvalidPhotoSuggestionResponse
	}
	var result PhotoSuggestion
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		row, err := scanPhotoSuggestion(tx.QueryRowContext(ctx, photoSuggestionRowSQL+` WHERE s.suggestion_id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		if row.dismissed || row.awaiting && accepted {
			return ErrPhotoSuggestionConflict
		}
		if accepted {
			receipt, err := s.acceptPhotoSuggestionTx(ctx, tx, &row, request)
			if err != nil {
				return err
			}
			row.AuthoredReceipt = receipt
		} else {
			row.Value, row.dismissed, row.awaiting = document.PhotoDescription{Tags: []string{}}, true, false
		}
		tags, err := json.Marshal(row.Value.Tags)
		if err != nil {
			return err
		}
		resolved, err := json.Marshal(row.resolved)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_suggestions SET caption=?,tags_json=?,visible_text=?,dismissed=?,awaiting=?,caption_resolved=?,resolved_tags_json=? WHERE id=?`,
			row.Value.Caption, string(tags), row.Value.VisibleText, row.dismissed, row.awaiting, row.captionResolved, string(resolved), row.id); err != nil {
			return err
		}
		// The response carries the revisions the accepted edit just produced.
		current, err := scanPhotoSuggestion(tx.QueryRowContext(ctx, photoSuggestionRowSQL+` WHERE s.id=?`, row.id))
		current.AuthoredReceipt = row.AuthoredReceipt
		result = current.PhotoSuggestion
		return err
	})
	return result, err
}

// acceptPhotoSuggestionTx applies the requested fields through the authored
// batch and moves them from pending to resolved on row.
func (s *Store) acceptPhotoSuggestionTx(ctx context.Context, tx *sql.Tx, row *photoSuggestionRow, request PhotoSuggestionResponse) (*PhotoAuthoredReceipt, error) {
	if request.Caption && row.Value.Caption == "" {
		return nil, ErrPhotoSuggestionConflict
	}
	for _, tag := range request.Tags {
		if !slices.Contains(row.Value.Tags, tag) {
			return nil, ErrPhotoSuggestionConflict
		}
	}
	edit := PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{{AssetID: request.AssetID, AssetRevision: request.AssetRevision, FileID: row.Target, Revision: request.SourceRevision, NodeRevision: request.NodeRevision}}}
	if request.Caption {
		edit.Patch.Caption = &row.Value.Caption
	}
	targets, loaded, err := s.photoAuthoredBatchTargetsTx(ctx, tx, edit, photoAuthoredTargetChecks{requireNodeRevision: true, hiddenUnlocked: row.hidden})
	if errors.Is(err, ErrStaleRevision) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidPhotoAsset) {
		return nil, ErrPhotoSuggestionConflict
	}
	if err != nil {
		return nil, err
	}
	for _, tag := range request.Tags {
		created, err := s.suggestedTagTx(ctx, tx, tag)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(edit.Keywords, func(edit PhotoKeywordEdit) bool { return edit.TagID == created }) {
			edit.Keywords = append(edit.Keywords, PhotoKeywordEdit{TagID: created, Assign: true})
		}
	}
	receipt, err := s.applyPhotoAuthoredBatchTx(ctx, tx, edit, targets, loaded)
	if err != nil {
		return nil, err
	}
	if request.Caption {
		row.Value.Caption, row.captionResolved = "", true
	}
	row.Value.Tags = slices.DeleteFunc(row.Value.Tags, func(tag string) bool { return slices.Contains(request.Tags, tag) })
	for _, tag := range request.Tags {
		if !slices.Contains(row.resolved, tag) {
			row.resolved = append(row.resolved, tag)
		}
	}
	if receipt.ReceiptID == "" {
		return nil, nil //nolint:nilnil // A nil receipt is the canonical result of an edit that changed nothing.
	}
	return &receipt, nil
}

// suggestedTagTx returns the ID of the tag named name, creating it when
// missing. Tags match by exact NFC name, as they do everywhere else, so a case
// variant of an existing tag becomes its own tag.
func (s *Store) suggestedTagTx(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name=?`, name).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	created, err := s.createTagTx(ctx, tx, name)
	return created.ID, err
}

// photoSuggestionsQuery lists the pending description of an asset's displayed photo.
func (s *Store) photoSuggestionsQuery(ctx context.Context, q metadataQuerier, assetID string) ([]PhotoSuggestion, error) {
	out := []PhotoSuggestion{}
	err := queryRows(ctx, q, photoSuggestionRowSQL+` WHERE a.asset_id=? AND a.display_file_id=f.file_id AND NOT s.dismissed AND NOT s.awaiting
 AND (s.caption<>'' OR s.tags_json<>'[]' OR s.visible_text<>'') ORDER BY s.created_at,s.suggestion_id`, []any{assetID}, func(rows *sql.Rows) error {
		row, err := scanPhotoSuggestion(rows)
		if err != nil {
			return err
		}
		out = append(out, row.PhotoSuggestion)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The owner may have typed or assigned a suggested value since it was stored.
	for i := range out {
		if _, err := dropOwnedPhotoFieldsTx(ctx, q, out[i].ContentVersionID, &out[i].Value); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(out, func(suggestion PhotoSuggestion) bool {
		value := suggestion.Value
		return value.Caption == "" && len(value.Tags) == 0 && value.VisibleText == ""
	}), nil
}

// photoDescriptionTargetsSQL lists one page of versions wanting a description,
// in version order after the cursor. It walks the ready grid previews in index
// order, so a page costs its own length rather than the library's. The CROSS
// JOINs keep that order. Arguments: recipe, cursor, profile, limit, producer, restored only.
// Active description work for any profile excludes a photo, so two providers never describe it at once.
var photoDescriptionTargetsSQL = `SELECT ` + contentVersionCols + ` FROM content_versions WHERE version_id IN (
 SELECT v.version_id FROM visual_preview_generations g
 CROSS JOIN content_versions v ON v.version_id=g.content_version_id AND v.blob_hash=g.source_sha256
 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 WHERE g.content_version_id>?2 AND g.recipe_fingerprint=?1 AND ` + photoDescriptionEligibility + ` AND g.state='ready' AND NOT ` + photoSuggestionSettledSQL("g.content_version_id", "?5", false) + `
 AND (NOT ?6 OR EXISTS(SELECT 1 FROM photo_suggestions s WHERE s.content_version_id=g.content_version_id AND s.awaiting))
 AND NOT EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.content_version_id=v.version_id
  AND (w.profile_fingerprint=?3 AND (w.state='waiting' AND NOT (j.state='failed' AND j.failure_code IN ('consent','stale_authority'))
    OR w.state='rejected' AND w.failure_code IN ('terminal','ambiguous'))
   OR w.state='waiting' AND j.state IN ('queued','retry_wait','running') AND NOT ` + originalFileWaiterSQL + `))
 ORDER BY g.content_version_id LIMIT ?4) ORDER BY version_id`

// PhotoDescriptionTargets lists displayed photos in version order after the
// cursor that want a description from profile, whose descriptions carry the
// producer fingerprint: eligible, with a ready grid preview, no suggestion row,
// and no waiting request or terminal failure. A restored row awaiting
// regeneration also wants one unless that producer was dismissed on it.
// RestoredOnly lists only those restored photos.
func (s *Store) PhotoDescriptionTargets(ctx context.Context, profile, producer, after string, limit int, restoredOnly bool) ([]ContentVersion, error) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, err
	}
	result := []ContentVersion{}
	err = queryRows(ctx, s.db, photoDescriptionTargetsSQL, []any{fingerprints.GridRecipe, after, profile, limit, producer, restoredOnly}, func(rows *sql.Rows) error {
		version, err := scanContentVersion(rows)
		if err != nil {
			return err
		}
		result = append(result, version)
		return nil
	})
	return result, err
}

// UnclaimedPhotoDescriptionWaiters counts waiting photos that consume admission
// slots: those a worker can claim now. A retry still in its backoff holds none.
func (s *Store) UnclaimedPhotoDescriptionWaiters(ctx context.Context, profile string, now time.Time) (int, error) {
	var count int
	at := now.UTC().Format(timestampLayout)
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.profile_fingerprint=? AND w.state='waiting' AND (j.state='queued' OR j.state='retry_wait' AND j.available_at<=? OR j.state='running' AND j.lease_expires_at<=?)`, profile, at, at).Scan(&count)
	return count, err
}

// PhotoDescriptionInputBinding scopes a description job to one content version
// and the verified preview it uploads. Identical bytes in another version never
// share a result. Automatic and explicit requests for the same version share one
// job; each waiter records which kind it is.
func PhotoDescriptionInputBinding(versionID, previewChecksum string) string {
	digest := sha256.Sum256([]byte("docbank:photo-description-input:v1\x00" + versionID + "\x00" + previewChecksum))
	return hex.EncodeToString(digest[:])
}

// photoDescriptionInputTx confirms the version is still eligible and its ready
// grid preview is the one the job's binding names.
func photoDescriptionInputTx(ctx context.Context, tx *sql.Tx, execution document.RenditionExecutionIdentityV1, versionID, sourceSHA256 string) error {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return err
	}
	var checksum string
	err = tx.QueryRowContext(ctx, `SELECT g.checksum FROM visual_preview_generations g WHERE g.content_version_id=? AND g.recipe_fingerprint=? AND g.source_sha256=? AND g.state='ready'`, versionID, fingerprints.GridRecipe, sourceSHA256).Scan(&checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRenditionJobStaleAuthority
	}
	if err != nil {
		return err
	}
	eligible, err := photoDescriptionEligibleTx(ctx, tx, versionID)
	if err != nil {
		return err
	}
	if !eligible || PhotoDescriptionInputBinding(versionID, checksum) != execution.Upload.InputBinding {
		return ErrRenditionJobStaleAuthority
	}
	return nil
}

// joinPhotoDescriptionTx finishes a description request's admission. A settled
// version publishes its waiter with no job and no provider call; otherwise the
// job runs, reopening if an earlier run completed without a stored result.
func joinPhotoDescriptionTx(ctx context.Context, tx *sql.Tx, jobID, waiterID, version, producer string, explicit bool, now string) (RenditionJob, RenditionJobWaiter, error) {
	settled, err := photoDescriptionSettledTx(ctx, tx, version, producer, explicit)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, err
	}
	if settled {
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters SET state='published',failure_code=NULL,updated_at=? WHERE waiter_id=?`, now, waiterID); err != nil {
			return RenditionJob{}, RenditionJobWaiter{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET state='completed',phase='published',updated_at=? WHERE job_id=? AND state='queued' AND NOT EXISTS(SELECT 1 FROM rendition_job_waiters WHERE job_id=? AND state='waiting')`, now, jobID, jobID); err != nil {
			return RenditionJob{}, RenditionJobWaiter{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET state='queued',phase='queued',claim_owner=NULL,lease_expires_at=NULL,available_at=?,
 provider_started=0,provider_resume_handle=NULL,execution_snapshot_json=NULL,selected_waiter_id=NULL,authorization_grant_id=NULL,
 authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,failure_code=NULL,updated_at=? WHERE job_id=? AND state='completed'`, now, now, jobID); err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, err
	}
	job, err := loadRenditionJobTx(ctx, tx, jobID)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, err
	}
	waiter, err := loadRenditionJobWaiterTx(ctx, tx, waiterID)
	return job, waiter, err
}
