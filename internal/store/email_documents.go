package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"path"
	"strings"
	"unicode/utf8"
)

var ErrEmailDocumentConflict = errors.New("email attachment document authority is retained or conflicts")
var ErrInvalidEmailDocumentRequest = errors.New("invalid email attachment document request")

// PublishEmailDocuments binds already verified durable part blobs to ordinary
// files. External callers use processing.PublishEmailDocuments to verify bytes
// while holding the blob mutation lease.
func (s *Store) PublishEmailDocuments(ctx context.Context, request document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	var result document.EmailDocumentPublicationReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.publishEmailDocumentsTx(ctx, tx, request)
		return err
	})
	return result, err
}

func (s *Store) publishEmailDocumentsTx(ctx context.Context, tx *sql.Tx, request document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	digest, err := document.EmailDocumentRequestDigest(request)
	if err != nil {
		return document.EmailDocumentPublicationReceipt{}, fmt.Errorf("%w: %w", ErrInvalidEmailDocumentRequest, err)
	}
	var receipt document.EmailDocumentPublicationReceipt
	err = func() error {
		old, err := loadEmailDocumentPublication(ctx, tx, request.OperationID)
		if err == nil {
			if old.Receipt.RequestDigest != digest {
				return ErrEmailDocumentConflict
			}
			receipt = old.Receipt
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		view, parts, err := emailDocumentSource(ctx, tx, request)
		if err != nil {
			return err
		}
		dir, err := liveDirTx(tx, request.DestinationID)
		if err != nil {
			return err
		}
		if dir.Revision != request.DestinationRevision {
			return ErrStaleRevision
		}
		reuse := map[string]document.EmailDocumentReuse{}
		for _, v := range request.Reuse {
			reuse[v.PartPath] = v
		}
		receipt = document.EmailDocumentPublicationReceipt{OperationID: request.OperationID, RequestDigest: digest, CreatedAt: nowRFC3339(), InventoryState: string(view.Evidence.Inventory.State), Relations: []document.EmailDocumentRelation{}}
		for i, p := range parts {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := document.EmailDocumentRelation{OperationID: request.OperationID, Order: i + 1, Parent: request.Parent, GenerationID: request.GenerationID, AttachmentID: request.AttachmentID, PartPath: p.Path, SiblingOrder: p.SiblingOrder, Filename: p.Filename.SafeName, Outcome: document.EmailDocumentPartOutcome(p)}
			chosen, hasReuse := reuse[p.Path]
			delete(reuse, p.Path)
			if rel.Outcome != "decoded" {
				if hasReuse {
					return ErrEmailDocumentConflict
				}
				receipt.Relations = append(receipt.Relations, rel)
				continue
			}
			if hasReuse {
				v, err := emailVersion(ctx, tx, chosen.Child.VersionID)
				if err != nil {
					return err
				}
				if documentIdentity(v) != chosen.Child || v.BlobHash != p.Payload.SHA256 || v.Size != p.Payload.Size {
					return ErrEmailDocumentConflict
				}
				node, err := nodeByIDTx(tx, v.NodeID)
				if err != nil {
					return err
				}
				if node.Revision != chosen.Revision || node.CurrentVersionID != v.ID || node.TrashedAt != nil {
					return ErrStaleRevision
				}
				rel.Child = new(chosen.Child)
			} else {
				name, err := emailDocumentFilename(p, request.OperationID, i+1)
				if err != nil {
					return err
				}
				media := "application/octet-stream"
				if p.Media.Declared != nil {
					media = *p.Media.Declared
				} else if p.Media.Detected != nil {
					media = *p.Media.Detected
				}
				created, err := s.createFileWithReceiptTx(ctx, tx, dir.ID, name, p.Payload.SHA256, p.Payload.Size, media)
				if err != nil {
					return err
				}
				rel.Child = new(documentIdentity(created.Version))
			}
			receipt.Relations = append(receipt.Relations, rel)
		}
		if len(reuse) != 0 {
			return ErrEmailDocumentConflict
		}
		return insertEmailDocumentPublication(ctx, tx, request, receipt)
	}()
	if err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	return receipt, nil
}
func documentIdentity(v ContentVersion) document.EmailDocumentIdentity {
	return document.EmailDocumentIdentity{NodeID: v.NodeID, VersionID: v.ID, SHA256: v.BlobHash, Size: v.Size}
}
func emailDocumentSource(ctx context.Context, q metadataQuerier, r document.EmailDocumentPublicationRequest) (EmailMetadataView, []document.EmailPartV1, error) {
	v, err := emailVersion(ctx, q, r.Parent.VersionID)
	if err != nil {
		return EmailMetadataView{}, nil, err
	}
	if documentIdentity(v) != r.Parent {
		return EmailMetadataView{}, nil, ErrEmailDocumentConflict
	}
	view, err := emailMetadataView(ctx, q, v, r.GenerationID)
	if err != nil {
		return view, nil, err
	}
	if view.Attachment.ID != r.AttachmentID {
		return view, nil, ErrEmailDocumentConflict
	}
	if view.Evidence.Inventory == nil || len(view.Evidence.Inventory.Parts) > document.EmailDocumentMaxParts {
		return view, nil, ErrEmailPartUnavailable
	}
	parts := document.EmailAttachmentParts(view.Evidence)
	var total int64
	for _, p := range parts {
		if p.Payload != nil {
			if p.Payload.Size < 0 || p.Payload.Size > 128<<20 {
				return view, nil, ErrEmailCorrupt
			}
			total += p.Payload.Size
		}
	}
	if total > 256<<20 {
		return view, nil, ErrEmailCorrupt
	}
	return view, parts, nil
}
func emailDocumentFilename(p document.EmailPartV1, operation string, order int) (string, error) {
	base, err := document.SafeEmailFilename(p.Filename.SafeName, p.Path)
	if err != nil {
		return "", err
	}
	ext := path.Ext(base)
	if len(ext) > 32 {
		ext = ""
	}
	stem := strings.TrimSuffix(base, ext)
	suffix := fmt.Sprintf("--%s-%04d", operation, order)
	budget := 240 - len(suffix) - len(ext)
	if len(stem) > budget {
		stem = stem[:budget]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
	}
	return NormalizeName(stem + suffix + ext)
}

type emailDocumentPublicationRecord struct {
	Request document.EmailDocumentPublicationRequest
	Receipt document.EmailDocumentPublicationReceipt
}

func loadEmailDocumentPublication(ctx context.Context, q metadataQuerier, id string) (emailDocumentPublicationRecord, error) {
	var v emailDocumentPublicationRecord
	var request, receipt []byte
	var digest, parent, attachment string
	err := q.QueryRowContext(ctx, `SELECT request_digest,parent_version_id,email_attachment_id,request_json,receipt_json FROM email_document_publications WHERE operation_id=?`, id).Scan(&digest, &parent, &attachment, &request, &receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if len(request) > document.EmailDocumentMaxJSONBytes || len(receipt) > document.EmailDocumentMaxJSONBytes {
		return v, ErrEmailCorrupt
	}
	if err = document.UnmarshalEmailDocumentJSON(request, &v.Request); err != nil {
		return v, err
	}
	if err = document.UnmarshalEmailDocumentJSON(receipt, &v.Receipt); err != nil {
		return v, err
	}
	if err = document.ValidateEmailDocumentReceipt(v.Receipt); err != nil {
		return v, err
	}
	want, err := document.EmailDocumentRequestDigest(v.Request)
	if err != nil {
		return v, err
	}
	if v.Request.OperationID != id || v.Receipt.OperationID != id || want != digest || v.Receipt.RequestDigest != digest || parent != v.Request.Parent.VersionID || attachment != v.Request.AttachmentID || len(v.Receipt.Relations) > document.EmailDocumentMaxParts {
		return v, ErrEmailCorrupt
	}
	return v, nil
}
func insertEmailDocumentPublication(ctx context.Context, tx *sql.Tx, request document.EmailDocumentPublicationRequest, receipt document.EmailDocumentPublicationReceipt) error {
	req, err := json.Marshal(request)
	if err != nil {
		return err
	}
	rec, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(req) > document.EmailDocumentMaxJSONBytes || len(rec) > document.EmailDocumentMaxJSONBytes {
		return ErrInvalidEmailDocumentRequest
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO email_document_publications(operation_id,request_digest,parent_version_id,email_attachment_id,request_json,receipt_json) VALUES(?,?,?,?,?,?)`, request.OperationID, receipt.RequestDigest, request.Parent.VersionID, request.AttachmentID, string(req), string(rec)); err != nil {
		return err
	}
	for _, r := range receipt.Relations {
		var child any
		if r.Child != nil {
			child = r.Child.VersionID
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO email_document_relations(operation_id,occurrence_order,child_version_id) VALUES(?,?,?)`, request.OperationID, r.Order, child); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EmailDocumentPublication(ctx context.Context, operationID string) (document.EmailDocumentPublicationReceipt, error) {
	v, err := loadEmailDocumentPublication(ctx, s.db, operationID)
	return v.Receipt, err
}

// RemoveEmailDocumentPublication releases one exact receipt and its references.
// Ordinary child files survive; callers explicitly give up this retry identity.
func (s *Store) RemoveEmailDocumentPublication(ctx context.Context, operationID, digest string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		v, err := loadEmailDocumentPublication(ctx, tx, operationID)
		if err != nil {
			return err
		}
		if v.Receipt.RequestDigest != digest {
			return ErrEmailDocumentConflict
		}
		var retained bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_transfer_receipts WHERE document_publication_id=?)`, operationID).Scan(&retained); err != nil {
			return err
		}
		if retained {
			return ErrMailboxConflict
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_documents WHERE json_extract(canonical_json,'$.attachment_inventory.operation_id')=?)`, operationID).Scan(&retained); err != nil {
			return err
		}
		if retained {
			return bundle.ErrRetained
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM email_document_publications WHERE operation_id=?`, operationID)
		return err
	})
}
