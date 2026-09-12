package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
)

const emailDocumentFailedState = "failed"

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
	var serving bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id JOIN rendition_builds b ON b.build_id=a.build_id JOIN rendition_lexical_generation_builds gb ON gb.build_id=b.build_id JOIN rendition_lexical_heads lh ON lh.generation_id=gb.generation_id WHERE h.content_version_id=? AND b.source_sha256=? AND b.partial_success=0 AND b.truncated=0)`, r.Child.VersionID, r.Child.SHA256).Scan(&serving)
	if err != nil {
		return "", "", err
	}
	if serving {
		return "indexed", "", nil
	}
	var state, failure string
	err = q.QueryRowContext(ctx, `SELECT j.state,COALESCE(j.failure_code,'') FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.content_version_id=? ORDER BY w.updated_at DESC,w.waiter_id DESC LIMIT 1`, r.Child.VersionID).Scan(&state, &failure)
	if err == nil {
		switch state {
		case emailDocumentFailedState, "operator_required":
			return emailDocumentFailedState, failure, nil
		default:
			return "pending", failure, nil
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
	return "decoded", "processing_not_requested", nil
}
