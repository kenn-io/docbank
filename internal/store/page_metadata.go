package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	metadataPageDocumentType  = "page_document"
	metadataPageRecipeType    = "page_recipe"
	metadataPageImageType     = "page_image"
	metadataPageJobType       = "page_render_job"
	metadataPageChecksumField = "checksum"
)

type metadataPageRecord struct {
	Type          string         `json:"type"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

func pageMetadataKeys(ctx context.Context, q metadataQuerier, query string) ([]string, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func exportPageMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	documents, err := pageMetadataKeys(ctx, q, `SELECT version_id FROM page_documents ORDER BY version_id`)
	if err != nil {
		return err
	}
	for _, id := range documents {
		d, err := loadPageDocument(ctx, q, id)
		if err != nil {
			return err
		}
		var hash string
		var size int64
		if err := q.QueryRowContext(ctx, `SELECT blob_hash,size FROM content_versions WHERE version_id=?`, id).Scan(&hash, &size); err != nil {
			return err
		}
		if d.Source.SHA256 != hash || d.Source.Size != size {
			return ErrPageConflict
		}
		b, checksum, err := document.MarshalPageDocumentV1(d)
		if err != nil {
			return err
		}
		if err := write(metadataPageRecord{Type: metadataPageDocumentType, CanonicalJSON: b, Checksum: checksum}); err != nil {
			return err
		}
	}
	recipes, err := pageMetadataKeys(ctx, q, `SELECT checksum FROM page_recipes ORDER BY checksum`)
	if err != nil {
		return err
	}
	for _, id := range recipes {
		r, err := loadPageRecipe(ctx, q, id)
		if err != nil {
			return err
		}
		b, checksum, err := document.MarshalPageRecipeV1(r)
		if err != nil {
			return err
		}
		if err := write(metadataPageRecord{Type: metadataPageRecipeType, CanonicalJSON: b, Checksum: checksum}); err != nil {
			return err
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT version_id,recipe_sha256,page FROM page_images ORDER BY version_id,recipe_sha256,page`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type imageKey struct {
		version, recipe string
		page            int
	}
	var images []imageKey
	for rows.Next() {
		var key imageKey
		if err := rows.Scan(&key.version, &key.recipe, &key.page); err != nil {
			_ = rows.Close()
			return err
		}
		images = append(images, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, key := range images {
		view, err := loadPageImage(ctx, q, key.version, key.recipe, key.page)
		if err != nil {
			return err
		}
		b, checksum, err := document.MarshalPageImageV1(view.Image)
		if err != nil {
			return err
		}
		if err := write(metadataPageRecord{Type: metadataPageImageType, CanonicalJSON: b, Checksum: checksum}); err != nil {
			return err
		}
	}
	jobs, err := pageMetadataKeys(ctx, q, `SELECT id FROM page_render_jobs ORDER BY id`)
	if err != nil {
		return err
	}
	pending := 0
	for _, id := range jobs {
		job, err := loadPageJob(ctx, q, `id=?`, id)
		if err != nil {
			return err
		}
		if err := validatePageJobClosure(ctx, q, job); err != nil {
			return err
		}
		if job.State == "queued" || job.State == "running" {
			pending++
		}
		if pending > 64 {
			return ErrPageLimit
		}
		var hash string
		var size, nodeID int64
		if err := q.QueryRowContext(ctx, `SELECT blob_hash,size,node_id FROM content_versions WHERE version_id=?`, job.Request.Source.VersionID).Scan(&hash, &size, &nodeID); err != nil {
			return err
		}
		if hash != job.Request.Source.SHA256 || size != job.Request.Source.Size || nodeID != job.Request.NodeID {
			return ErrPageConflict
		}
		b, err := canonical.Marshal(job)
		if err != nil {
			return err
		}
		if err := write(metadataPageRecord{Type: metadataPageJobType, CanonicalJSON: b, Checksum: pageChecksum(b)}); err != nil {
			return err
		}
	}
	return nil
}

func importPageMetadata(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	var record metadataPageRecord
	if err := decodeMetadataRecord(raw, &record); err != nil {
		return err
	}
	if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum {
		return ErrPageConflict
	}
	switch kind {
	case metadataPageDocumentType:
		d, _, err := document.DecodePageDocumentV1(record.CanonicalJSON)
		if err != nil {
			return err
		}
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM page_documents WHERE version_id=?)`, d.Source.VersionID).Scan(&found); err != nil {
			return err
		}
		if found {
			return ErrPageConflict
		}
		return putPageDocument(ctx, tx, d)
	case metadataPageRecipeType:
		_, hash, err := document.DecodePageRecipeV1(record.CanonicalJSON)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO page_recipes(checksum,canonical_json) VALUES(?,?)`, hash, []byte(record.CanonicalJSON))
		return err
	case metadataPageImageType:
		image, hash, err := document.DecodePageImageV1(record.CanonicalJSON)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO page_images(version_id,page,recipe_sha256,canonical_json,checksum,blob_hash) VALUES(?,?,?,?,?,?)`, image.Source.VersionID, image.Page, image.RecipeSHA256, []byte(record.CanonicalJSON), hash, image.SHA256)
		return err
	case metadataPageJobType:
		job, err := canonical.Decode[PageRenderJob](record.CanonicalJSON)
		if err != nil {
			return err
		}
		if err := validatePageJob(job); err != nil {
			return err
		}
		if job.State == "running" {
			job.State = "queued"
		}
		request, err := canonical.Marshal(job.Request)
		if err != nil {
			return err
		}
		results, err := canonical.Marshal(job.Results)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO page_render_jobs(id,node_id,version_id,request_sha256,request_json,state,results_json,failure_code,epoch,token,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,0,'',?,?)`, job.ID, job.Request.NodeID, job.Request.Source.VersionID, job.RequestSHA256, request, job.State, results, job.FailureCode, job.CreatedAt, job.UpdatedAt)
		return err
	}
	return ErrPageConflict
}
