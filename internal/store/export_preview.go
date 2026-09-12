package store

import (
	"context"

	"go.kenn.io/docbank/document/bundle"
)

// ExportPlanPreview reads only immutable receipts in bounded pages; current
// heads and rendition providers have no role in this projection.
func (s *Store) ExportPlanPreview(ctx context.Context, owner, id string) (bundle.PlanPreview, error) {
	var out bundle.PlanPreview
	if owner == "" || validateUUIDv4(id) != nil {
		return out, bundle.ErrConflict
	}
	p, err := s.ExportPlan(ctx, owner, id)
	if err != nil {
		return out, err
	}
	if err = validateExportPolicies(p.Roles); err != nil {
		return out, err
	}
	out = bundle.PlanPreview{PlanID: p.ID, Fingerprint: p.Fingerprint, MemberHash: p.Source.MemberHash, Total: p.Total}
	indices := map[string]int{}
	for i, policy := range p.Roles {
		indices[policy.Role] = i
		out.Roles = append(out.Roles, bundle.RoleSummary{Role: policy.Role})
	}
	members, files, bytes := 0, 0, int64(0)
	err = s.WalkExportDocuments(ctx, id, func(d bundle.Document) error {
		members++
		if members > p.Total || members > bundle.MaxMembers {
			return bundle.ErrConflict
		}
		available, unavailable := map[string]bool{}, map[string]bool{}
		for _, r := range d.Roles {
			i, ok := indices[r.Role]
			if !ok {
				return bundle.ErrConflict
			}
			summary := &out.Roles[i]
			switch r.Status {
			case exportRoleAvailable:
				if r.Size < 0 || r.Size > bundle.MaxRoleBytes-bytes || files >= bundle.MaxRoles {
					return bundle.ErrLimit
				}
				available[r.Role] = true
				summary.Files++
				summary.Bytes += r.Size
				files++
				bytes += r.Size
			case exportRoleUnavailable:
				if unavailable[r.Role] || !p.Roles[i].AllowUnavailable {
					return bundle.ErrConflict
				}
				unavailable[r.Role] = true
			default:
				return bundle.ErrConflict
			}
		}
		for i := range out.Roles {
			r := &out.Roles[i]
			if available[r.Role] == unavailable[r.Role] {
				return bundle.ErrConflict
			}
			if available[r.Role] {
				r.AvailableMembers++
			} else {
				r.UnavailableMembers++
			}
		}
		return nil
	})
	if err != nil {
		return bundle.PlanPreview{}, err
	}
	if members != p.Total || files != p.RoleEntries || bytes != p.RoleBytes {
		return bundle.PlanPreview{}, bundle.ErrConflict
	}
	for i := range out.Roles {
		r := &out.Roles[i]
		if r.UnavailableMembers == 0 {
			continue
		}
		switch r.Role {
		case "text":
			r.UnavailableReason = "No eligible retained text in the frozen plan."
		case "pages":
			r.UnavailableReason = "No complete retained page recipe in the frozen plan."
		default:
			r.UnavailableReason = "Unavailable in the frozen plan."
		}
	}
	return out, nil
}
