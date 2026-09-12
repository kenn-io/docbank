package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
)

const exportInventoryComplete = "complete"

func expandExportAttachments(ctx context.Context, q metadataQuerier, parent bundle.Document, policies []bundle.RolePolicy, selected string, write func(bundle.Document) error) error {
	var attachments []bundle.RolePolicy
	for _, p := range policies {
		if p.Role == "attachment_original" || p.Role == "attachment_pdf" {
			attachments = append(attachments, p)
		}
	}
	if len(attachments) == 0 {
		if selected != "" {
			return bundle.ErrConflict
		}
		return write(parent)
	}
	publication, err := exportAttachmentPublication(ctx, q, parent.VersionID, selected)
	if selected != "" && errors.Is(err, ErrNotFound) {
		return bundle.ErrConflict
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		parent.Inventory = &bundle.AttachmentInventory{State: "unavailable"}
	} else {
		if err = validateEmailDocumentPublication(ctx, q, publication); err != nil {
			return err
		}
		p, r := publication.Request, publication.Receipt
		if p.Parent != (document.EmailDocumentIdentity{NodeID: parent.NodeID, VersionID: parent.VersionID, SHA256: parent.SHA256, Size: parent.Size}) {
			return bundle.ErrConflict
		}
		parent.Inventory = &bundle.AttachmentInventory{OperationID: p.OperationID, RequestDigest: r.RequestDigest, GenerationID: p.GenerationID, AttachmentID: p.AttachmentID, State: r.InventoryState, Total: len(r.Relations)}
	}
	if parent.Inventory.State != exportInventoryComplete {
		for _, policy := range attachments {
			if !policy.AllowUnavailable {
				return fmt.Errorf("%w: attachment inventory for %d/%s is %s", bundle.ErrUnavailable, parent.NodeID, parent.VersionID, parent.Inventory.State)
			}
		}
	}
	if err = write(parent); err != nil {
		return err
	}
	for _, relation := range publication.Receipt.Relations {
		d := bundle.Document{Member: parent.Member, Name: parent.Name, Path: parent.Path, MediaType: parent.MediaType, Attachment: new(relation), Roles: []bundle.Role{}}
		for _, policy := range attachments {
			role := bundle.Role{Role: policy.Role, Status: exportRoleUnavailable, Reason: relation.Outcome}
			if relation.Child != nil {
				child, e := emailVersion(ctx, q, relation.Child.VersionID)
				if e != nil {
					return e
				}
				if documentIdentity(child) != *relation.Child {
					return bundle.ErrConflict
				}
				base := fmt.Sprintf("documents/%d/%s/attachments/%04d/", parent.NodeID, parent.VersionID, relation.Order)
				if policy.Role == "attachment_original" {
					role = bundle.Role{Role: policy.Role, Status: exportRoleAvailable, Path: base + "original", SHA256: child.BlobHash, Size: child.Size, MediaType: child.MimeType}
				} else {
					role, e = resolveExportEmailPDF(ctx, q, child.ID, policy, base)
					if e != nil && !errors.Is(e, ErrNotFound) && !errors.Is(e, sql.ErrNoRows) {
						return e
					}
					if e != nil {
						role = bundle.Role{Status: exportRoleUnavailable, Reason: "No qualified retained email PDF for this child and recipe."}
					}
					role.Role = policy.Role
				}
			}
			if role.Status == exportRoleUnavailable && !policy.AllowUnavailable {
				return fmt.Errorf("%w: %s for %d/%s part %s: %s", bundle.ErrUnavailable, policy.Role, parent.NodeID, parent.VersionID, relation.PartPath, role.Reason)
			}
			d.Roles = append(d.Roles, role)
		}
		if err = write(d); err != nil {
			return err
		}
	}
	return nil
}

func exportAttachmentPublication(ctx context.Context, q metadataQuerier, version, selected string) (emailDocumentPublicationRecord, error) {
	if selected == "" {
		rows, err := q.QueryContext(ctx, `SELECT operation_id FROM email_document_publications WHERE parent_version_id=? ORDER BY operation_id LIMIT 2`, version)
		if err != nil {
			return emailDocumentPublicationRecord{}, err
		}
		defer func() { _ = rows.Close() }()
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return emailDocumentPublicationRecord{}, err
		}
		if len(ids) == 0 {
			return emailDocumentPublicationRecord{}, ErrNotFound
		}
		if len(ids) != 1 {
			return emailDocumentPublicationRecord{}, fmt.Errorf("%w: select an attachment publication for %s", bundle.ErrConflict, version)
		}
		selected = ids[0]
	}
	p, err := loadEmailDocumentPublication(ctx, q, selected)
	if err == nil && p.Request.Parent.VersionID != version {
		return p, bundle.ErrConflict
	}
	return p, err
}

func validateExportAttachmentAuthority(ctx context.Context, q metadataQuerier, d bundle.Document) error {
	if d.Inventory == nil && d.Attachment == nil {
		return nil
	}
	var id string
	if d.Inventory != nil {
		id = d.Inventory.OperationID
	} else {
		id = d.Attachment.OperationID
	}
	if id == "" {
		return nil
	}
	p, err := loadEmailDocumentPublication(ctx, q, id)
	if err != nil {
		return err
	}
	if p.Request.Parent != (document.EmailDocumentIdentity{NodeID: d.NodeID, VersionID: d.VersionID, SHA256: d.SHA256, Size: d.Size}) {
		return bundle.ErrConflict
	}
	if d.Inventory != nil {
		i := d.Inventory
		if i.Total != len(p.Receipt.Relations) || i.State != p.Receipt.InventoryState || i.RequestDigest != p.Receipt.RequestDigest || i.GenerationID != p.Request.GenerationID || i.AttachmentID != p.Request.AttachmentID {
			return bundle.ErrConflict
		}
	} else {
		r := d.Attachment
		if r.Order < 1 || r.Order > len(p.Receipt.Relations) || !reflect.DeepEqual(*r, p.Receipt.Relations[r.Order-1]) {
			return bundle.ErrConflict
		}
	}
	return nil
}
