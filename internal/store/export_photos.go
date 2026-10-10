package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

type PhotoExportInput struct {
	Name          string        `json:"name"`
	Member        bundle.Member `json:"member"`
	FileID        string        `json:"file_id"`
	AssetRevision int64         `json:"asset_revision"`
	FileRevision  int64         `json:"file_revision"`
	NodeRevision  int64         `json:"node_revision"`
	MediaType     string        `json:"media_type"`
	Authored      PhotoAuthored `json:"authored"`
	Keywords      []string      `json:"keywords"`
}

type PreparedPhotoExport struct {
	Input    PhotoExportInput
	Receipt  bundle.PhotoRenderReceipt
	SHA256   string
	Size     int64
	Physical BlobPhysical
}

func (a PreparedPhotoExport) role() (bundle.Role, error) {
	raw, err := canonical.Marshal(a.Receipt)
	return bundle.Role{Role: "photo_rendered", Status: roleAvailable, Path: bundle.PhotoRenderedPath(a.Receipt.Source, a.Receipt.Profile), SHA256: a.SHA256, Size: a.Size, MediaType: "image/" + a.Receipt.Profile.Format, Recipe: raw}, err
}

func validatePhotoPlanRequest(r *bundle.PlanRequest) error {
	photo := slices.ContainsFunc(r.Roles, func(p bundle.RolePolicy) bool { return p.Role == "photo_rendered" })
	if photo != (r.PhotoRender != nil) {
		return bundle.ErrConflict
	}
	if photo && (len(r.Roles) != 1 || r.Roles[0] != (bundle.RolePolicy{Role: "photo_rendered"}) || len(r.Publications) != 0) {
		return bundle.ErrConflict
	}
	if r.PhotoRender != nil {
		if err := r.PhotoRender.Validate(); err != nil {
			return err
		}
		profile := r.PhotoRender.Canonical()
		r.PhotoRender = &profile
	}
	return nil
}

// ExportPlanReplay checks identity before expensive rendering and again at seal.
func (s *Store) ExportPlanReplay(ctx context.Context, owner string, r bundle.PlanRequest) (bundle.Plan, bool, error) {
	raw, _, err := validateExportPlanRequest(owner, &r)
	if err != nil {
		return bundle.Plan{}, false, err
	}
	plan, found, err := replayExportPlan(ctx, s.db, owner, r.OperationID, pageChecksum(raw))
	if !found && err == nil {
		err = checkExportPlanCapacity(ctx, s.db)
	}
	return plan, found, err
}

// ResolvePhotoExportMembers uses the same complete population as Photos browsing.
func (s *Store) ResolvePhotoExportMembers(ctx context.Context, selection bundle.PhotoExportSelection) ([]bundle.Member, string, error) {
	if len(selection.AssetIDs) > bundle.MaxPhotoExportMembers {
		return nil, "", fmt.Errorf("%w: photo exports allow at most %d photos", bundle.ErrLimit, bundle.MaxPhotoExportMembers)
	}
	selected := make(map[string]bool, len(selection.AssetIDs))
	for _, id := range selection.AssetIDs {
		if validateUUIDv4(id) != nil || selected[id] {
			return nil, "", bundle.ErrConflict
		}
		selected[id] = true
	}
	var out []bundle.Member
	var cursor *PhotoBrowsePosition
	identity := ""
	for {
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: selection.Query, Hidden: selection.Hidden, PageSize: MaxDocumentCatalogPageSize, assetIDs: selection.AssetIDs}, cursor)
		if err != nil {
			return nil, "", err
		}
		if len(selection.AssetIDs) == 0 && page.Total > bundle.MaxPhotoExportMembers {
			return nil, "", fmt.Errorf("%w: photo exports allow at most %d photos", bundle.ErrLimit, bundle.MaxPhotoExportMembers)
		}
		if len(page.Items) > 0 {
			identity = page.Items[0].position.QueryIdentity
		}
		for _, row := range page.Items {
			delete(selected, row.AssetID)
			m := bundle.Member{NodeID: row.NodeID, VersionID: row.ContentVersionID}
			if err := s.db.QueryRowContext(ctx, `SELECT v.blob_hash,v.size,n.revision FROM content_versions v JOIN nodes n ON n.id=v.node_id WHERE v.version_id=? AND n.current_version_id=v.version_id`, m.VersionID).Scan(&m.SHA256, &m.Size, &m.Revision); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, "", fmt.Errorf("%w: photo %d is no longer exportable", bundle.ErrConflict, m.NodeID)
				}
				return nil, "", err
			}
			if len(out) >= bundle.MaxPhotoExportMembers {
				return nil, "", bundle.ErrLimit
			}
			out = append(out, m)
		}
		if page.Next == nil || len(selection.AssetIDs) > 0 && len(selected) == 0 {
			break
		}
		cursor = page.Next
		identity = cursor.QueryIdentity
	}
	if len(selected) != 0 {
		return nil, "", bundle.ErrConflict
	}
	if identity != "" {
		identity = "sha256:" + identity
	}
	return out, identity, nil
}

func (s *Store) exportPhotoInput(ctx context.Context, q metadataQuerier, m bundle.Member) (i PhotoExportInput, err error) {
	i = PhotoExportInput{Member: m, Keywords: []string{}}
	defer func() {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("%w: photo %d is no longer exportable", bundle.ErrConflict, m.NodeID)
		} else if err != nil {
			err = fmt.Errorf("photo %d: %w", m.NodeID, err)
		}
	}()
	var hidden sql.NullString
	err = q.QueryRowContext(ctx, `SELECT f.file_id,f.revision,a.revision,n.revision,v.mime_type,a.hidden_at,n.name FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id WHERE n.id=? AND v.version_id=? AND v.blob_hash=? AND v.size=? AND a.display_file_id=f.file_id AND a.excluded_at IS NULL AND n.trashed_at IS NULL`, m.NodeID, m.VersionID, m.SHA256, m.Size).Scan(&i.FileID, &i.FileRevision, &i.AssetRevision, &i.NodeRevision, &i.MediaType, &hidden, &i.Name)
	if err != nil {
		return i, err
	}
	if m.Revision != 0 && m.Revision != i.NodeRevision {
		return i, bundle.ErrConflict
	}
	if hidden.Valid {
		if _, err := s.hiddenSession(ctx, q); err != nil {
			return i, err
		}
	}
	f, err := photoFileByIDQuery(ctx, q, i.FileID)
	if err != nil {
		return i, err
	}
	i.Authored = f.Authored()
	rows, err := q.QueryContext(ctx, `SELECT t.name FROM node_tags nt JOIN tags t ON t.id=nt.tag_id WHERE nt.node_id=? ORDER BY t.name,t.id`, m.NodeID)
	if err != nil {
		return i, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return i, err
		}
		i.Keywords = append(i.Keywords, name)
		if len(i.Keywords) > 1000 {
			return i, bundle.ErrLimit
		}
	}
	return i, rows.Err()
}

func (s *Store) ExportPhotoInputs(ctx context.Context, owner string, r bundle.PlanRequest) ([]PhotoExportInput, error) {
	source, err := s.ExportSource(ctx, owner, r.SourceID)
	if err != nil {
		return nil, err
	}
	if source.Total > bundle.MaxPhotoExportMembers {
		return nil, fmt.Errorf("%w: photo exports allow at most %d photos", bundle.ErrLimit, bundle.MaxPhotoExportMembers)
	}
	if source.State != "sealed" || source.MemberHash != r.MemberHash {
		return nil, bundle.ErrConflict
	}
	var inputs []PhotoExportInput
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return walkExportMembers(ctx, tx, r.SourceID, func(m bundle.Member) error {
			i, err := s.exportPhotoInput(ctx, tx, m)
			if err != nil {
				return err
			}
			inputs = append(inputs, i)
			return nil
		})
	})
	return inputs, err
}

// SealPhotoExportPlan accepts artifacts only from the in-process renderer.
func (s *Store) SealPhotoExportPlan(ctx context.Context, owner string, r bundle.PlanRequest, artifacts []PreparedPhotoExport) (bundle.Plan, error) {
	prepared := make(map[string]PreparedPhotoExport, len(artifacts))
	for _, a := range artifacts {
		if _, exists := prepared[a.Receipt.Source.VersionID]; exists {
			return bundle.Plan{}, bundle.ErrConflict
		}
		if a.Input.Member != a.Receipt.Source {
			return bundle.Plan{}, bundle.ErrConflict
		}

		prepared[a.Receipt.Source.VersionID] = a
	}
	return s.createExportPlan(ctx, owner, r, prepared)
}

// AcquirePhotoExportPreparation bounds rendering for this vault before loading source pixels.
func (s *Store) AcquirePhotoExportPreparation(ctx context.Context) (func(), error) {
	select {
	case s.photoExportSlot <- struct{}{}:
		return func() { <-s.photoExportSlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
