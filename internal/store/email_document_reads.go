package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
)

func (s *Store) EmailDocumentRelations(ctx context.Context, query document.EmailDocumentRelationQuery) (document.EmailDocumentRelationPage, error) {
	query, err := document.NormalizeEmailDocumentRelationQuery(query)
	if err != nil {
		return document.EmailDocumentRelationPage{}, fmt.Errorf("%w: %w", ErrInvalidEmailDocumentRequest, err)
	}
	page := document.EmailDocumentRelationPage{Items: []document.EmailDocumentRelationStatus{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	where := "p.parent_version_id=?"
	id := query.ParentVersionID
	if query.ChildVersionID != "" {
		where = "r.child_version_id=?"
		id = query.ChildVersionID
	}
	from := ` FROM email_document_relations r JOIN email_document_publications p ON p.operation_id=r.operation_id WHERE ` + where
	if err = tx.QueryRowContext(ctx, `SELECT count(*)`+from, id).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.operation_id,r.occurrence_order`+from+` AND (r.operation_id>? OR (r.operation_id=? AND r.occurrence_order>?)) ORDER BY r.operation_id,r.occurrence_order LIMIT ?`, id, query.AfterOperationID, query.AfterOperationID, query.AfterOrder, query.Limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	type key struct {
		id    string
		order int
	}
	keys := []key{}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.order); err != nil {
			break
		}
		keys = append(keys, k)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	if len(keys) > query.Limit {
		keys = keys[:query.Limit]
		last := keys[len(keys)-1]
		page.NextOperationID = last.id
		page.NextOrder = last.order
	}
	var last emailDocumentPublicationRecord
	for _, k := range keys {
		if last.Request.OperationID != k.id {
			last, err = loadEmailDocumentPublication(ctx, tx, k.id)
			if err != nil {
				return page, err
			}
		}
		if k.order < 1 || k.order > len(last.Receipt.Relations) {
			return page, ErrEmailCorrupt
		}
		rel := last.Receipt.Relations[k.order-1]
		state, reason, err := emailDocumentProcessingState(ctx, tx, rel)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, document.EmailDocumentRelationStatus{Relation: rel, State: state, Reason: reason})
	}
	return page, nil
}
func emailDocumentProcessingState(ctx context.Context, q metadataQuerier, r document.EmailDocumentRelation) (string, string, error) {
	if r.Child == nil {
		return r.Outcome, "mime_" + r.Outcome, nil
	}
	generation, err := collectionGenerationTx(ctx, q)
	if err != nil {
		return "", "", err
	}
	profile, err := legacyPlainTextProfile()
	if err != nil {
		return "", "", err
	}
	var state, failure string
	err = q.QueryRowContext(ctx, `WITH
	 coverage_versions AS (SELECT node_id,version_id,blob_hash FROM content_versions WHERE version_id=? AND blob_hash=?),
	 coverage_selection AS (
	  SELECT profile_fingerprint profile,? generation FROM rendition_heads WHERE content_version_id=?
	  UNION SELECT ?,?
	 ), `+processingVersionCoverageCTE()+`
	 SELECT state FROM processing_coverage WHERE state IN ('complete','partial','none')
	 ORDER BY CASE state WHEN 'complete' THEN 0 WHEN 'partial' THEN 1 ELSE 2 END LIMIT 1`,
		r.Child.VersionID, r.Child.SHA256, generation, r.Child.VersionID, profile.Fingerprint, generation).Scan(&state)
	if err == nil {
		if state == "complete" {
			state = "indexed"
		}
		return state, "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	err = q.QueryRowContext(ctx, `SELECT CASE WHEN w.state='rejected' THEN 'failed' ELSE j.state END,
	 COALESCE(w.failure_code,j.failure_code,'')
	 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
	 WHERE w.content_version_id=?
	 ORDER BY MAX(w.updated_at,j.updated_at) DESC,w.updated_at DESC,j.updated_at DESC,w.waiter_id DESC,j.job_id DESC LIMIT 1`, r.Child.VersionID).Scan(&state, &failure)
	if err == nil {
		switch state {
		case string(RenditionJobFailed), "operator_required":
			return string(RenditionJobFailed), failure, nil
		case "queued", "running", "retry_wait":
			return "pending", failure, nil
		case "completed":
			return "decoded", "rendition_not_serving", nil
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	var queued bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM text_extraction_queue WHERE blob_hash=?)`, r.Child.SHA256).Scan(&queued)
	if err != nil {
		return "", "", err
	}
	if queued {
		return "pending", "text_extraction", nil
	}
	err = q.QueryRowContext(ctx, `SELECT e.status FROM text_searchable_versions sv
	 JOIN content_versions v ON v.version_id=sv.version_id JOIN extracted_text e ON e.blob_hash=v.blob_hash
	 WHERE sv.version_id=? ORDER BY e.status='ok' DESC,e.extracted_at DESC,e.extractor DESC LIMIT 1`, r.Child.VersionID).Scan(&state)
	if err == nil {
		if state == ExtractionFailed {
			return ExtractionFailed, "text_extraction_failed", nil
		}
		if generation == "" {
			return "none", "empty_text", nil
		}
		return "decoded", "text_extraction_not_serving", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	return "decoded", "processing_not_requested", nil
}
