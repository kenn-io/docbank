package bundle

import (
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const MaxDocumentRows = MaxMembers + MaxRoles

func (p Plan) Rows() int {
	if p.DocumentRows == 0 {
		return p.Total
	}
	return p.DocumentRows
}

// RowValidator reconciles the ordered parent/occurrence stream with bounded
// state. Writers, independent readers and backup validation share this contract.
type RowValidator struct {
	Plan                    Plan
	Counts                  OutputCounts
	parent                  Document
	rows, members, children int
	paths                   map[string]bool
	volumes                 VolumeCursor
	duplicates              DuplicateCursor
}

func (v *RowValidator) finishParent() error {
	if v.parent.Inventory != nil && v.children != v.parent.Inventory.Total {
		return ErrConflict
	}
	return nil
}

func (v *RowValidator) Add(d Document) error {
	if v.rows == 0 {
		v.volumes.Limits = v.Plan.VolumeLimits
		v.duplicates.Policy = v.Plan.DuplicatePolicy
	}
	if err := v.duplicates.Add(&d, false); err != nil {
		return err
	}
	if err := v.count(d); err != nil {
		return err
	}
	if err := ValidateVolumeLimits(v.Plan.VolumeLimits); err != nil {
		return err
	}
	for _, r := range d.Roles {
		if r.Status != "available" {
			if r.Volume != 0 {
				return ErrConflict
			}
			continue
		}
		index, err := v.volumes.Add(r.Size)
		if err != nil {
			return err
		}
		if r.Volume != index {
			return ErrConflict
		}
	}
	if v.rows >= v.Plan.Rows() || v.rows >= MaxDocumentRows || d.NodeID < 1 {
		return ErrConflict
	}
	v.rows++
	if d.Attachment == nil {
		if v.finishParent() != nil || v.members > 0 && (d.NodeID < v.parent.NodeID || d.NodeID == v.parent.NodeID && d.VersionID <= v.parent.VersionID) {
			return ErrConflict
		}
		v.parent, v.children, v.paths = d, 0, map[string]bool{}
		v.members++
		if err := ValidateEmailPDFRoles(v.Plan, d); err != nil {
			return err
		}
		hasPolicy := false
		for _, p := range v.Plan.Roles {
			if p.Role != "attachment_original" && p.Role != "attachment_pdf" {
				continue
			}
			hasPolicy = true
			if d.Inventory == nil || d.Inventory.State != "complete" && !p.AllowUnavailable {
				return ErrConflict
			}
		}
		if hasPolicy != (d.Inventory != nil) {
			return ErrConflict
		}
		if i := d.Inventory; i != nil {
			if i.Total < 0 || i.Total > document.EmailDocumentMaxParts {
				return ErrLimit
			}
			if i.State == "unavailable" {
				if *i != (AttachmentInventory{State: "unavailable"}) {
					return ErrConflict
				}
			} else if (i.State != "complete" && i.State != "partial") || document.ValidateEmailDocumentOperationID(i.OperationID) != nil || !canonical.IsSHA256Hex(i.RequestDigest) || !canonical.IsSHA256Hex(i.GenerationID) || !canonical.IsSHA256Hex(i.AttachmentID) {
				return ErrConflict
			}
		}
		for _, r := range d.Roles {
			if r.Role == "attachment_original" || r.Role == "attachment_pdf" {
				return ErrConflict
			}
		}
		return nil
	}
	if v.members == 0 || d.Inventory != nil || d.Member != v.parent.Member || d.Name != v.parent.Name || d.Path != v.parent.Path || d.MediaType != v.parent.MediaType || len(d.Frames) != 0 {
		return ErrConflict
	}
	i, rel := v.parent.Inventory, d.Attachment
	if i == nil || v.children >= i.Total || rel.Order != v.children+1 || rel.OperationID != i.OperationID || rel.GenerationID != i.GenerationID || rel.AttachmentID != i.AttachmentID || rel.Parent != (document.EmailDocumentIdentity{NodeID: d.NodeID, VersionID: d.VersionID, SHA256: d.SHA256, Size: d.Size}) || v.paths[rel.PartPath] {
		return ErrConflict
	}
	v.paths[rel.PartPath] = true
	v.children++
	// Reuse the publication validator for the individual occurrence's shape.
	shape := *rel
	shape.Order = 1
	if document.ValidateEmailDocumentReceipt(document.EmailDocumentPublicationReceipt{OperationID: i.OperationID, RequestDigest: i.RequestDigest, CreatedAt: v.Plan.CreatedAt, InventoryState: i.State, Relations: []document.EmailDocumentRelation{shape}}) != nil {
		return ErrConflict
	}
	index := 0
	for _, p := range v.Plan.Roles {
		if p.Role != "attachment_original" && p.Role != "attachment_pdf" {
			continue
		}
		if index >= len(d.Roles) || d.Roles[index].Role != p.Role {
			return ErrConflict
		}
		r := d.Roles[index]
		index++
		if r.Status == "collapsed" {
			r.Status = "available"
		}
		if r.Page != nil {
			return ErrConflict
		}
		if r.Status == "unavailable" {
			if !p.AllowUnavailable || r.Path != "" || r.SHA256 != "" || r.Size != 0 || len(r.Recipe) != 0 || r.MediaType != "" || r.Reason == "" || len(r.Reason) > 512 {
				return ErrConflict
			}
			continue
		}
		if r.Status != "available" || rel.Child == nil || r.Reason != "" {
			return ErrConflict
		}
		base := fmt.Sprintf("documents/%d/%s/attachments/%04d/", d.NodeID, d.VersionID, rel.Order)
		if p.Role == "attachment_original" {
			if r.Path != base+"original" || r.SHA256 != rel.Child.SHA256 || r.Size != rel.Child.Size || len(r.Recipe) != 0 {
				return ErrConflict
			}
		} else {
			if r.Path != base+"email.pdf" {
				return ErrConflict
			}
			p.Role, r.Role = "email_pdf", "email_pdf"
			r.Path = fmt.Sprintf("documents/%d/%s/email.pdf", rel.Child.NodeID, rel.Child.VersionID)
			child := Document{NodeID: rel.Child.NodeID, VersionID: rel.Child.VersionID, SHA256: rel.Child.SHA256, Size: rel.Child.Size, Roles: []Role{r}}
			if ValidateEmailPDFRoles(Plan{Roles: []RolePolicy{p}}, child) != nil {
				return ErrConflict
			}
		}
	}
	if index != len(d.Roles) {
		return ErrConflict
	}
	return nil
}

func (v *RowValidator) Finish() error {
	if v.rows != v.Plan.Rows() || v.members != v.Plan.Total || v.volumes.Index != v.Plan.Volumes || v.Plan.Counts != nil && v.Counts != *v.Plan.Counts {
		return ErrConflict
	}
	return v.finishParent()
}

func (v *RowValidator) count(d Document) error {
	if d.Attachment != nil {
		v.Counts.Attachments++
	} else if d.MediaType == "message/rfc822" {
		v.Counts.Messages++
	}
	if d.Inventory != nil && d.Inventory.State != "complete" {
		v.Counts.UnavailableInventories++
	}
	for _, r := range d.Roles {
		switch r.Status {
		case "unavailable":
			v.Counts.Unavailable++
		case "collapsed":
			v.Counts.Collapsed++
		}
		if r.Role != "email_pdf" && r.Role != "attachment_pdf" || r.Status == "unavailable" {
			continue
		}
		var receipt document.EmailPDFReceiptV1
		if json.Unmarshal(r.Recipe, &receipt, json.RejectUnknownMembers(true)) != nil {
			return ErrConflict
		}
		if d.Inventory != nil && d.Inventory.OperationID != "" && d.Inventory.GenerationID != receipt.Binding.GenerationID {
			return ErrConflict
		}
		if r.Status == "available" {
			if r.Role == "email_pdf" {
				v.Counts.EmailPDFs++
			} else {
				v.Counts.AttachmentPDFs++
			}
			v.Counts.Pages += receipt.Output.Pages
		}
	}
	return nil
}
