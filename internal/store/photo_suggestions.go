package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	photoSuggestionKind      = "photo_description"
	photoSuggestionPending   = "pending"
	photoSuggestionAccepted  = "accepted"
	photoSuggestionDismissed = "dismissed"
)

// photoSuggestionOfVersionSQL selects the proposal belonging to one source version.
func photoSuggestionOfVersionSQL(version string) string {
	return `SELECT s.suggestion_id FROM photo_suggestions s WHERE s.content_version_id=` + version + ` AND s.kind='photo_description'`
}

// photoSuggestionSettledSQL holds a photo's suggestion that the producer must
// not replace: an owner decision, or a pending proposal from the same producer.
func photoSuggestionSettledSQL(version, producer string) string {
	return `EXISTS(` + photoSuggestionOfVersionSQL(version) + ` AND (s.response<>'pending' OR s.producer_fingerprint=` + producer + `))`
}

// Photo descriptions publish a waiter outcome instead of a rendition head.
// Later waiters can reopen the shared job without changing a published outcome.
var photoDescriptionPublishedSQL = `w.state='published' AND json_extract(j.execution_identity_json,'$.upload.input_kind')='derived_upload'`

var ErrInvalidPhotoSuggestionResponse = errors.New("invalid photo suggestion response")

var ErrPhotoSuggestionConflict = errors.New("photo suggestion revision changed")

type PhotoSuggestionAcceptedTag struct {
	FileID string `json:"file_id"`
	TagID  string `json:"tag_id"`
}

type PhotoSuggestion struct {
	AcceptedTags        []PhotoSuggestionAcceptedTag `json:"-"`
	AuthoredReceipt     *PhotoAuthoredReceipt        `json:"authored_receipt,omitempty"`
	AuthoredReceiptID   string                       `json:"authored_receipt_id,omitempty"`
	ID                  string                       `json:"suggestion_id"`
	ContentVersionID    string                       `json:"content_version_id"`
	ProducerFingerprint string                       `json:"producer_fingerprint"`
	Kind                string                       `json:"kind"`
	Target              string                       `json:"target"`
	Value               document.PhotoDescription    `json:"value"`
	Response            string                       `json:"response"`
	Revision            int64                        `json:"revision"`
	ReceiptID           *string                      `json:"receipt_id,omitempty"`
	CreatedAt           string                       `json:"created_at"`
}

const photoSuggestionColumns = `s.suggestion_id,s.content_version_id,s.producer_fingerprint,s.kind,(SELECT f.file_id FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=s.content_version_id),s.value_json,s.response,s.revision,s.receipt_id,s.created_at,s.accepted_tags_json`

func scanPhotoSuggestion(row interface{ Scan(dest ...any) error }) (PhotoSuggestion, error) {
	var value PhotoSuggestion
	var raw, tags string
	if err := row.Scan(&value.ID, &value.ContentVersionID, &value.ProducerFingerprint, &value.Kind, &value.Target, &raw, &value.Response, &value.Revision, &value.ReceiptID, &value.CreatedAt, &tags); err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(tags), &value.AcceptedTags); err != nil {
		return value, err
	}
	return value, json.Unmarshal([]byte(raw), &value.Value, json.RejectUnknownMembers(true))
}

var photoDescriptionEligibility = liveIncludedPhotoDisplayPredicate("v") + ` AND a.hidden_at IS NULL`

func photoDescriptionEligibleTx(ctx context.Context, q metadataQuerier, versionID string) (bool, error) {
	var eligible bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=? AND `+photoDescriptionEligibility+`)`, versionID).Scan(&eligible)
	return eligible, err
}
func (s *Store) PhotoDescriptionEligible(ctx context.Context, versionID string) (bool, error) {
	return photoDescriptionEligibleTx(ctx, s.db, versionID)
}
func (s *Store) PhotoSuggestions(ctx context.Context, assetID string) ([]PhotoSuggestion, error) {
	out := []PhotoSuggestion{}
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		_, err := s.photoAssetReadQuery(ctx, tx, assetID)
		if err != nil {
			return err
		}
		out, err = s.photoSuggestionsQuery(ctx, tx, assetID)
		return err
	})
	return out, err
}

// PublishPhotoDescription finishes the provider job and its proposal together,
// without constructing a searchable rendition or retaining provider artifacts.
func (s *Store) PublishPhotoDescription(ctx context.Context, claim RenditionJobClaim, result document.RenditionResult, at time.Time) error {
	if len(result.Artifacts) != 1 || result.Artifacts[0].Role != document.EvidenceArtifactStructured {
		return ErrInvalidRenditionJobRequest
	}
	var description document.PhotoDescription
	if err := json.Unmarshal(result.Artifacts[0].Payload, &description, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var publicationErr error
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		if job.Phase != RenditionPhaseProvider {
			return ErrRenditionJobStaleAuthority
		}
		authority, err := s.authorizeRenditionJobPublicationTx(ctx, tx, job, at)
		if err != nil {
			return err
		}
		publicationErr = authority.authorizationErr
		if publicationErr != nil {
			// A rejected description has no staged artifact to reuse. Keep the
			// existing fresh-attempt path available to authorized waiters.
			_, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET phase='queued',provider_started=0,provider_resume_handle=NULL,provider_attempts=0,execution_snapshot_json=NULL,selected_waiter_id=NULL,authorization_grant_id=NULL,authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,updated_at=? WHERE job_id=? AND claim_epoch=?`, at.UTC().Format(timestampLayout), job.ID, claim.Epoch)
			return err
		}
		for _, authorized := range authority.authorized {
			waiter := authorized.waiter

			if err := putPhotoSuggestionTx(ctx, tx, waiter.ContentVersionID, job.RenditionRequestFingerprint, description, at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters SET state=?,updated_at=? WHERE waiter_id=?`, "published", at.UTC().Format(timestampLayout), waiter.ID); err != nil {
				return err
			}
		}
		return completeRenditionPublicationTx(ctx, tx, claim, at.UTC().Format(timestampLayout))
	})
	if err != nil {
		return err
	}
	return publicationErr
}

// putPhotoSuggestionTx gives the photo owning version its one pending proposal. A
// pending proposal from another producer is refreshed in place under a new
// revision; owner decisions and same-producer proposals stay unchanged. An empty
// result retires pending output without changing an owner decision.
func putPhotoSuggestionTx(ctx context.Context, tx *sql.Tx, version, producer string, description document.PhotoDescription, at time.Time) error {
	var decided bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(`+photoSuggestionOfVersionSQL("?")+` AND s.response<>'pending')`, version).Scan(&decided); err != nil {
		return err
	}
	if decided {
		return nil
	}
	if description.Caption == "" && len(description.Tags) == 0 && description.VisibleText == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM photo_suggestions WHERE response='pending' AND suggestion_id IN (`+photoSuggestionOfVersionSQL("?")+`)`, version)
		return err
	}
	raw, err := json.Marshal(description)
	if err != nil {
		return err
	}
	value := string(raw)
	if _, err := tx.ExecContext(ctx, `UPDATE photo_suggestions SET producer_fingerprint=?2,value_json=?3,revision=revision+1 WHERE response='pending' AND producer_fingerprint<>?2 AND suggestion_id IN (`+photoSuggestionOfVersionSQL("?1")+`)`, version, producer, value); err != nil {
		return err
	}
	suggestionID, err := newUUIDv4()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,kind,value_json,response,revision,created_at) SELECT ?2,?1,?3,'photo_description',?4,'pending',1,?5 FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE n.current_version_id=?1 AND NOT EXISTS(`+photoSuggestionOfVersionSQL("?1")+`)`, version, suggestionID, producer, value, at.UTC().Format(timestampLayout))
	return err
}

type PhotoSuggestionResponse struct {
	Targets  []PhotoAuthoredEditTarget `json:"targets,omitempty"`
	Revision int64                     `json:"revision"`
	Response string                    `json:"response"`
	Caption  bool                      `json:"caption"`
	Tags     []string                  `json:"tags"`
}

func (s *Store) RespondPhotoSuggestion(ctx context.Context, id string, request PhotoSuggestionResponse) (PhotoSuggestion, error) {
	var result PhotoSuggestion
	if request.Response != photoSuggestionAccepted && request.Response != photoSuggestionDismissed || request.Response == photoSuggestionDismissed && (request.Caption || len(request.Tags) > 0) {
		return result, ErrInvalidPhotoSuggestionResponse
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		value, err := scanPhotoSuggestion(tx.QueryRowContext(ctx, `SELECT `+photoSuggestionColumns+` FROM photo_suggestions s WHERE s.suggestion_id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var assetID string
		var hidden bool
		if err := tx.QueryRowContext(ctx, `SELECT a.asset_id,a.hidden_at IS NOT NULL FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE f.file_id=? AND n.current_version_id=? AND n.trashed_at IS NULL`, value.Target, value.ContentVersionID).Scan(&assetID, &hidden); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		if value.Revision != request.Revision {
			return ErrStaleRevision
		}
		if value.Response == photoSuggestionDismissed {
			return ErrPhotoSuggestionConflict
		}
		if request.Caption && value.Value.Caption == "" {
			return ErrInvalidPhotoSuggestionResponse
		}
		var edit PhotoAuthoredBatch
		var targets []PhotoAuthoredTarget
		var loaded map[string]photoAuthoredLoaded
		if request.Response == photoSuggestionAccepted {
			if len(request.Targets) == 0 {
				return ErrInvalidPhotoSuggestionResponse
			}
			if !request.Caption && len(request.Tags) == 0 {
				return ErrInvalidPhotoSuggestionResponse
			}
			edit = PhotoAuthoredBatch{Targets: request.Targets}
			if request.Caption {
				edit.Patch.Caption = &value.Value.Caption
			}
			for _, target := range request.Targets {
				if target.AssetID != assetID {
					return ErrPhotoSuggestionConflict
				}
			}
			targets, loaded, err = s.photoAuthoredBatchTargetsTx(ctx, tx, edit, true)
			if errors.Is(err, ErrStaleRevision) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidPhotoAsset) {
				return ErrPhotoSuggestionConflict
			}
			if err != nil {
				return err
			}
			for _, tag := range request.Tags {
				if !slices.Contains(value.Value.Tags, tag) {
					return ErrInvalidPhotoSuggestionResponse
				}
				tagID, err := s.ensureSuggestionTagTx(ctx, tx, tag)
				if err != nil {
					return err
				}
				edit.Keywords = append(edit.Keywords, PhotoKeywordEdit{TagID: tagID, Assign: true})
			}
			captionCurrent := request.Caption
			tagsCurrent := make([]bool, len(request.Tags))
			for i := range tagsCurrent {
				tagsCurrent[i] = true
			}
			for _, target := range request.Targets {
				snapshot := loaded[target.FileID]
				captionCurrent = captionCurrent && snapshot.file.Caption == value.Value.Caption
				for i, keyword := range edit.Keywords {
					var assigned bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM node_tags WHERE node_id=? AND tag_id=?)`, snapshot.node.ID, keyword.TagID).Scan(&assigned); err != nil {
						return err
					}
					tagsCurrent[i] = tagsCurrent[i] && assigned
				}
			}
			if (!request.Caption || captionCurrent) && !slices.Contains(tagsCurrent, false) {
				return fmt.Errorf("%w: selected fields are already applied", ErrInvalidPhotoSuggestionResponse)
			}
		}
		value.Response = request.Response
		value.Revision++
		var authoredReceipt *PhotoAuthoredReceipt
		if request.Response == photoSuggestionAccepted {
			authored, err := s.applyPhotoAuthoredBatchTx(ctx, tx, edit, targets, loaded)
			if err != nil {
				return err
			}
			for _, target := range request.Targets {
				for _, keyword := range edit.Keywords {
					accepted := PhotoSuggestionAcceptedTag{FileID: target.FileID, TagID: keyword.TagID}
					if !slices.Contains(value.AcceptedTags, accepted) {
						value.AcceptedTags = append(value.AcceptedTags, accepted)
					}
				}
			}
			slices.SortFunc(value.AcceptedTags, comparePhotoSuggestionAcceptedTags)
			value.AuthoredReceiptID = authored.ReceiptID
			if value.ReceiptID == nil {
				value.ReceiptID = &authored.ReceiptID
			}
			authoredReceipt = &authored
		}

		tags, err := json.Marshal(value.AcceptedTags)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_suggestions SET response=?,revision=?,receipt_id=?,accepted_tags_json=? WHERE suggestion_id=?`, value.Response, value.Revision, value.ReceiptID, string(tags), id); err != nil {
			return err
		}
		result = value
		result.AuthoredReceipt = authoredReceipt
		return nil
	})
	return result, err
}
func (s *Store) ensureSuggestionTagTx(ctx context.Context, tx *sql.Tx, name string) (tagID string, retErr error) {
	name, err := NormalizeTagName(name)
	if err != nil {
		return "", err
	}
	tag, err := tagByNameQuery(ctx, tx, name)
	if err == nil {
		return tag.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	// Tag names stay case-sensitive for authored edits. AI spelling reuses
	// the first equivalent identity without renaming the owner's tag.
	rows, err := tx.QueryContext(ctx, `SELECT id,name FROM tags ORDER BY name COLLATE BINARY,id`)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var equivalent string
	for rows.Next() {
		var id, existing string
		if err := rows.Scan(&id, &existing); err != nil {
			return "", err
		}
		if equivalent == "" && strings.EqualFold(existing, name) {
			equivalent = id
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if equivalent != "" {
		return equivalent, nil
	}
	tag, err = s.createTagTx(ctx, tx, name)
	return tag.ID, err
}

func validatePhotoSuggestion(value PhotoSuggestion) error {
	if value.Response == photoSuggestionPending && len(value.AcceptedTags) > 0 {
		return errors.New("invalid accepted suggestion tags")
	}
	for i, accepted := range value.AcceptedTags {
		if validateUUIDv4(accepted.FileID) != nil || validateUUIDv4(accepted.TagID) != nil || i > 0 && comparePhotoSuggestionAcceptedTags(value.AcceptedTags[i-1], accepted) >= 0 {
			return errors.New("invalid accepted suggestion tag identity")
		}
	}

	if validateUUIDv4(value.ID) != nil || validateUUIDv4(value.ContentVersionID) != nil || value.Kind != photoSuggestionKind || validateCatalogSHA256(value.ProducerFingerprint, "suggestion producer") != nil || !slices.Contains([]string{photoSuggestionPending, photoSuggestionAccepted, photoSuggestionDismissed}, value.Response) || value.Revision < 1 {
		return errors.New("invalid photo suggestion")
	}
	if value.AuthoredReceiptID != "" && validateUUIDv4(value.AuthoredReceiptID) != nil {
		return errors.New("invalid authored receipt reference")
	}
	if value.Response == photoSuggestionPending && value.ReceiptID != nil || value.ReceiptID != nil && validateUUIDv4(*value.ReceiptID) != nil {
		return errors.New("invalid photo suggestion response receipt")
	}
	if err := document.ValidatePhotoDescription(value.Value); err != nil {
		return err
	}
	return validateMetadataTime("photo suggestion created_at", value.CreatedAt)
}

// PhotoDescriptionTargets lists photos without a settled proposal for the
// profile, without retrying terminal work.
func (s *Store) PhotoDescriptionTargets(ctx context.Context, profile, after string, limit int) ([]ContentVersion, error) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, err
	}
	result := []ContentVersion{}
	err = queryRows(ctx, s.db, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id IN (SELECT v.version_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id JOIN visual_preview_generations g ON g.content_version_id=v.version_id AND g.recipe_fingerprint=?1 AND g.source_sha256=v.blob_hash WHERE v.version_id>?2 AND `+photoDescriptionEligibility+` AND g.state='ready' AND NOT `+photoSuggestionSettledSQL("v.version_id", "(SELECT rendition_request_fingerprint FROM processing_profiles WHERE profile_fingerprint=?3)")+` AND NOT EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.content_version_id=v.version_id AND w.profile_fingerprint=?3 AND (w.state='published' OR w.state='rejected' AND w.failure_code IN ('terminal','ambiguous') OR j.state<>'completed' AND NOT (w.state='published' AND (j.state='failed' AND j.failure_code='terminal' OR j.state='operator_required'))) AND NOT (w.state='rejected' AND w.failure_code='stale_authority') AND NOT (j.state='failed' AND j.phase IN ('queued','build_staged','generation_staged') AND j.failure_code IN ('consent','stale_authority'))) ORDER BY v.version_id LIMIT ?4) ORDER BY version_id`, []any{fingerprints.GridRecipe, after, profile, limit}, func(rows *sql.Rows) error {
		version, err := scanContentVersion(rows)
		if err != nil {
			return err
		}
		result = append(result, version)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (s *Store) photoSuggestionsQuery(ctx context.Context, q metadataQuerier, assetID string) ([]PhotoSuggestion, error) {
	out := []PhotoSuggestion{}
	rows, err := q.QueryContext(ctx, `SELECT `+photoSuggestionColumns+` FROM photo_suggestions s JOIN content_versions v ON v.version_id=s.content_version_id JOIN photo_files f ON f.node_id=v.node_id JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE a.asset_id=? AND a.display_file_id=f.file_id AND n.current_version_id=s.content_version_id AND n.trashed_at IS NULL ORDER BY s.created_at,s.suggestion_id`, assetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value, err := scanPhotoSuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// Undo changes authored state, so responses based on its previous revision must refresh.
func bumpUndonePhotoSuggestionRevisionsTx(ctx context.Context, tx *sql.Tx, receipt PhotoAuthoredReceipt) error {
	assets := make(map[string]bool)
	for _, before := range receipt.Before {
		assetID, _, err := photoAssetOwningNodeTx(ctx, tx, before.NodeID)
		if err != nil {
			return err
		}
		if assetID == "" || assets[assetID] {
			continue
		}
		assets[assetID] = true
		if _, err := tx.ExecContext(ctx, `UPDATE photo_suggestions SET revision=revision+1 WHERE response='accepted' AND content_version_id IN (SELECT v.version_id FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE f.asset_id=?)`, assetID); err != nil {
			return err
		}
	}
	return nil
}

func comparePhotoSuggestionAcceptedTags(a, b PhotoSuggestionAcceptedTag) int {
	if result := strings.Compare(a.FileID, b.FileID); result != 0 {
		return result
	}
	return strings.Compare(a.TagID, b.TagID)
}
