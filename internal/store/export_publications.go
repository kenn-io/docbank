package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

// ExportAttachmentPublications lists ambiguous sets, including publications with
// no attachments. Planning independently checks and pins the selected receipt.
func (s *Store) ExportAttachmentPublications(ctx context.Context, owner, id string, after int) (bundle.AttachmentPublications, error) {
	var out bundle.AttachmentPublications
	if owner == "" || validateUUIDv4(id) != nil || after < 0 || after > bundle.MaxRoles {
		return out, bundle.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	source, err := loadExportSource(ctx, tx, owner, id)
	if err != nil {
		return out, err
	}
	if exportExpired(source.ExpiresAt) {
		return out, bundle.ErrExpired
	}
	if source.State != "sealed" {
		return out, bundle.ErrConflict
	}
	out = bundle.AttachmentPublications{SourceID: id, MemberHash: source.MemberHash, After: after, Items: []bundle.AttachmentPublicationChoice{}}
	err = walkExportMembers(ctx, tx, id, func(m bundle.Member) error {
		var count int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM email_document_publications WHERE parent_version_id=?`, m.VersionID).Scan(&count); e != nil {
			return e
		}
		if count < 2 {
			return nil
		}
		offset := max(0, after-out.Total)
		out.Total += count
		if out.Total > bundle.MaxRoles {
			return bundle.ErrLimit
		}
		if offset >= count || len(out.Items) == bundle.InspectionPageSize {
			return nil
		}
		rows, e := tx.QueryContext(ctx, `SELECT operation_id FROM email_document_publications WHERE parent_version_id=? ORDER BY operation_id LIMIT ? OFFSET ?`, m.VersionID, bundle.InspectionPageSize-len(out.Items), offset)
		if e != nil {
			return e
		}
		defer func() { _ = rows.Close() }()
		var ids []string
		for rows.Next() {
			var operation string
			if e = rows.Scan(&operation); e != nil {
				break
			}
			ids = append(ids, operation)
		}
		if e = errors.Join(e, rows.Err(), rows.Close()); e != nil {
			return e
		}
		var name string
		if e = tx.QueryRowContext(ctx, `SELECT name FROM nodes WHERE id=?`, m.NodeID).Scan(&name); e != nil {
			return e
		}
		for _, operation := range ids {
			p, e := loadEmailDocumentPublication(ctx, tx, operation)
			if e != nil {
				return e
			}
			out.Items = append(out.Items, bundle.AttachmentPublicationChoice{NodeID: m.NodeID, VersionID: m.VersionID, Name: name, OperationID: operation, GenerationID: p.Request.GenerationID, CreatedAt: p.Receipt.CreatedAt, State: p.Receipt.InventoryState, Attachments: len(p.Receipt.Relations)})
		}
		return nil
	})
	if err != nil {
		return bundle.AttachmentPublications{}, err
	}
	if after > out.Total {
		return bundle.AttachmentPublications{}, bundle.ErrConflict
	}
	if after+len(out.Items) < out.Total {
		out.Next = after + len(out.Items)
	}
	raw, err := canonical.Marshal(out)
	if err != nil {
		return bundle.AttachmentPublications{}, err
	}
	if len(raw) > bundle.MaxMemberBytes {
		return bundle.AttachmentPublications{}, bundle.ErrLimit
	}
	if err = tx.Commit(); err != nil {
		return bundle.AttachmentPublications{}, err
	}
	return out, nil
}
