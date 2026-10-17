package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/query"
)

const maxPhotoExportKeywordBytes = 1 << 20

type PhotoExportInput struct {
	Name          string        `json:"name"`
	Member        bundle.Member `json:"member"`
	FileID        string        `json:"file_id"`
	AssetRevision int64         `json:"asset_revision"`
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
	if photo && (r.DuplicatePolicy != "" && r.DuplicatePolicy != "preserve" || r.VolumeLimits != nil) {
		return fmt.Errorf("%w: photo exports take no duplicate_policy or volume_limits", bundle.ErrConflict)
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

// ResolvePhotoExportMembers resolves a validated selection against the Photos browsing population.
func (s *Store) ResolvePhotoExportMembers(ctx context.Context, selection bundle.PhotoExportSelection) ([]bundle.Member, string, error) {
	selected := make(map[string]bool, len(selection.AssetIDs))
	for _, id := range selection.AssetIDs {
		selected[id] = true
	}
	// Membership ignores order, and relevance ranking skips the selected-asset
	// restriction, so a search selection resolves in capture order.
	if selection.Query.Sort.Field == "relevance" {
		selection.Query.Sort = query.Sort{Field: "capture_time", Direction: "desc"}
	}
	var out []bundle.Member
	identity := ""
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: selection.Query, Hidden: selection.Hidden, PageSize: MaxDocumentCatalogPageSize, assetIDs: selection.AssetIDs}, nil)
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
		out = append(out, m)
	}
	if len(selected) != 0 {
		for _, id := range selection.AssetIDs {
			if !selected[id] {
				continue
			}
			var nodeID int64
			var name string
			err := s.db.QueryRowContext(ctx, `SELECT n.id,n.name FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE f.asset_id=? AND `+photoVisibilityPredicate(selection.Hidden)+` ORDER BY f.file_id=a.display_file_id DESC,f.file_id LIMIT 1`, id).Scan(&nodeID, &name)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, "", fmt.Errorf("photo %s: %w: no longer exportable", id, bundle.ErrConflict)
			}
			if err != nil {
				return nil, "", err
			}
			return nil, "", fmt.Errorf("photo %d (%s): %w: no longer exportable", nodeID, name, bundle.ErrConflict)
		}
	}
	if identity != "" {
		identity = "sha256:" + identity
	}
	return out, identity, nil
}

func photoExportSourceRequest(ctx context.Context, q metadataQuerier, source bundle.Source) (bundle.SourceRequest, error) {
	if source.Kind != "photos" {
		return bundle.SourceRequest{}, nil
	}
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT request_json FROM export_sources WHERE id=?`, source.ID).Scan(&raw); err != nil {
		return bundle.SourceRequest{}, err
	}
	var request bundle.SourceRequest
	if err := json.Unmarshal(raw, &request, json.RejectUnknownMembers(true)); err != nil {
		return bundle.SourceRequest{}, err
	}
	return request, nil
}

func (s *Store) exportPhotoInput(ctx context.Context, q metadataQuerier, m bundle.Member, selection *bundle.PhotoExportSelection, includeMetadata bool) (i PhotoExportInput, err error) {
	i = PhotoExportInput{Member: m, Keywords: []string{}}
	defer func() {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("%w: invalid or no longer exportable member", bundle.ErrConflict)
		}
		if err != nil {
			if i.Name != "" {
				err = fmt.Errorf("photo %d (%s): %w", m.NodeID, i.Name, err)
			} else {
				err = fmt.Errorf("photo %d: %w", m.NodeID, err)
			}
		}
	}()
	var hidden sql.NullString
	var display sql.NullBool
	var name string
	var current bundle.Member
	err = q.QueryRowContext(ctx, `SELECT f.file_id,a.revision,n.revision,v.mime_type,a.hidden_at,n.name,a.display_file_id=f.file_id,v.version_id,v.blob_hash,v.size FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id WHERE n.id=? AND a.excluded_at IS NULL AND n.trashed_at IS NULL`, m.NodeID).Scan(&i.FileID, &i.AssetRevision, &i.NodeRevision, &i.MediaType, &hidden, &name, &display, &current.VersionID, &current.SHA256, &current.Size)
	if err != nil {
		return i, err
	}
	if selection != nil && selection.Hidden != hidden.Valid {
		return i, fmt.Errorf("%w: photo left the selected scope", bundle.ErrConflict)
	}
	if hidden.Valid {
		if _, err := s.hiddenSession(ctx, q); err != nil {
			return i, err
		}
	}
	i.Name = name
	if current.VersionID != m.VersionID || current.SHA256 != m.SHA256 || current.Size != m.Size || m.Revision != 0 && m.Revision != i.NodeRevision {
		return i, fmt.Errorf("%w: source changed or is no longer exportable", bundle.ErrConflict)
	}
	if !display.Bool {
		return i, fmt.Errorf("%w: member isn't the photo's display file", bundle.ErrConflict)
	}
	f, err := photoFileByIDQuery(ctx, q, i.FileID)
	if err != nil {
		return i, err
	}
	i.Authored = f.Authored()
	if !includeMetadata {
		return i, nil
	}
	i.Keywords, err = photoKeywordNames(ctx, q, m.NodeID)
	return i, err
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
	err = s.photoReadTx(ctx, func(tx *sql.Tx) error {
		request, err := photoExportSourceRequest(ctx, tx, source)
		if err != nil {
			return err
		}
		return walkExportMembers(ctx, tx, r.SourceID, func(m bundle.Member) error {
			i, err := s.exportPhotoInput(ctx, tx, m, request.Photos, r.PhotoRender != nil && r.PhotoRender.IncludeMetadata)
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

// photoKeywordNames is the ordered portable keyword snapshot for a member.
func photoKeywordNames(ctx context.Context, q metadataQuerier, nodeID int64) ([]string, error) {
	names := []string{}
	keywordBytes := 0
	err := queryRows(ctx, q, `SELECT t.name FROM node_tags nt JOIN tags t ON t.id=nt.tag_id WHERE nt.node_id=? ORDER BY t.name,t.id`, []any{nodeID}, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		keywordBytes += len(name)
		if keywordBytes > maxPhotoExportKeywordBytes {
			return bundle.ErrLimit
		}
		names = append(names, name)
		return nil
	})
	return names, err
}
