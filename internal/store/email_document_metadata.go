package store

import (
	"context"
	"database/sql"
	"errors"
	"go.kenn.io/docbank/document"
	"reflect"
)

type metadataEmailDocumentPublication struct {
	Type    string                                   `json:"type"`
	Request document.EmailDocumentPublicationRequest `json:"request"`
	Receipt document.EmailDocumentPublicationReceipt `json:"receipt"`
}

func exportEmailDocumentMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	// Page keys so each receipt stays bounded without loading all publications.
	after := ""
	for {
		var id string
		err := q.QueryRowContext(ctx, `SELECT operation_id FROM email_document_publications WHERE operation_id>? ORDER BY operation_id LIMIT 1`, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		v, err := loadEmailDocumentPublication(ctx, q, id)
		if err != nil {
			return err
		}
		if err = validateEmailDocumentPublication(ctx, q, v); err != nil {
			return err
		}
		if err = write(metadataEmailDocumentPublication{Type: "email_document_publication", Request: v.Request, Receipt: v.Receipt}); err != nil {
			return err
		}
		after = id
	}
}
func validateEmailDocumentPublication(ctx context.Context, q metadataQuerier, v emailDocumentPublicationRecord) error {
	digest, err := document.EmailDocumentRequestDigest(v.Request)
	if err != nil {
		return err
	}
	if digest != v.Receipt.RequestDigest || v.Receipt.OperationID != v.Request.OperationID {
		return ErrEmailCorrupt
	}
	if err = validateMetadataTime("email document publication created_at", v.Receipt.CreatedAt); err != nil {
		return err
	}
	view, parts, err := emailDocumentSource(ctx, q, v.Request)
	if err != nil {
		return err
	}
	if len(parts) != len(v.Receipt.Relations) || v.Receipt.InventoryState != string(view.Evidence.Inventory.State) {
		return ErrEmailCorrupt
	}
	reuse := map[string]document.EmailDocumentReuse{}
	for _, r := range v.Request.Reuse {
		reuse[r.PartPath] = r
	}
	for i, p := range parts {
		r := v.Receipt.Relations[i]
		expected := document.EmailDocumentRelation{OperationID: v.Request.OperationID, Order: i + 1, Parent: v.Request.Parent, GenerationID: v.Request.GenerationID, AttachmentID: v.Request.AttachmentID, PartPath: p.Path, SiblingOrder: p.SiblingOrder, Filename: p.Filename.SafeName, Outcome: document.EmailDocumentPartOutcome(p), Child: r.Child}
		if !reflect.DeepEqual(r, expected) || (r.Child != nil) != (r.Outcome == "decoded") {
			return ErrEmailCorrupt
		}
		if r.Child != nil {
			if err = document.ValidateEmailDocumentIdentity(*r.Child); err != nil {
				return err
			}
			child, err := emailVersion(ctx, q, r.Child.VersionID)
			if err != nil {
				return err
			}
			if documentIdentity(child) != *r.Child || child.BlobHash != p.Payload.SHA256 || child.Size != p.Payload.Size {
				return ErrEmailCorrupt
			}
			if chosen, ok := reuse[p.Path]; ok {
				if chosen.Child != *r.Child {
					return ErrEmailCorrupt
				}
				delete(reuse, p.Path)
			}
		}
		var linked sql.NullString
		if err = q.QueryRowContext(ctx, `SELECT child_version_id FROM email_document_relations WHERE operation_id=? AND occurrence_order=?`, r.OperationID, r.Order).Scan(&linked); err != nil {
			return err
		}
		if linked.Valid != (r.Child != nil) || (r.Child != nil && linked.String != r.Child.VersionID) {
			return ErrEmailCorrupt
		}
	}
	if len(reuse) != 0 {
		return ErrEmailCorrupt
	}
	var count int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM email_document_relations WHERE operation_id=?`, v.Request.OperationID).Scan(&count); err != nil {
		return err
	}
	if count != len(parts) {
		return ErrEmailCorrupt
	}
	return nil
}
