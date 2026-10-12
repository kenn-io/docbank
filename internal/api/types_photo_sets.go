package api

import "go.kenn.io/docbank/internal/store"

type PhotoAlbum struct {
	ID           string  `json:"id" format:"uuid"`
	Name         string  `json:"name" maxLength:"256"`
	Starred      bool    `json:"starred"`
	Revision     int64   `json:"revision" minimum:"1"`
	CoverAssetID *string `json:"cover_asset_id,omitzero" format:"uuid"`
	CreatedAt    string  `json:"created_at" format:"date-time"`
	UpdatedAt    string  `json:"updated_at" format:"date-time"`
	DeletedAt    *string `json:"deleted_at,omitzero" format:"date-time"`
}

type PhotoAlbumSummary struct {
	PhotoAlbum

	MemberCount           int64   `json:"member_count" minimum:"0"`
	IncludedCount         int64   `json:"included_count" minimum:"0"`
	EffectiveCoverAssetID *string `json:"effective_cover_asset_id,omitzero" format:"uuid"`
	CoverGenerationID     *string `json:"cover_generation_id,omitzero"`
}

type PhotoAlbumNameRequest struct {
	Name string `json:"name" minLength:"1" maxLength:"256"`
}
type UpdatePhotoAlbumRequest struct {
	Name    *string `json:"name,omitzero" minLength:"1" maxLength:"256"`
	Starred *bool   `json:"starred,omitzero"`
}
type PhotoAlbumCoverRequest struct {
	AssetID *string `json:"asset_id,omitzero" format:"uuid"`
}
type PhotoAlbumMembersRequest struct {
	AssetIDs []string               `json:"asset_ids,omitzero" maxItems:"1000" format:"uuid"`
	Query    *QueryPayload          `json:"query,omitzero"`
	Coverage WorkspaceQueryCoverage `json:"coverage,omitzero"`
}
type photoAlbumOutput struct {
	ETag string `header:"ETag"`
	Body PhotoAlbum
}

func fromStorePhotoAlbum(v store.PhotoSet) PhotoAlbum {
	return PhotoAlbum{ID: v.ID, Name: v.Name, Starred: v.Starred, Revision: v.Revision, CoverAssetID: v.CoverAssetID, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, DeletedAt: v.DeletedAt}
}
func fromStorePhotoAlbumSummary(v store.PhotoSetSummary) PhotoAlbumSummary {
	return PhotoAlbumSummary{PhotoAlbum: fromStorePhotoAlbum(v.PhotoSet), MemberCount: v.MemberCount, IncludedCount: v.IncludedCount, EffectiveCoverAssetID: v.EffectiveCoverAssetID, CoverGenerationID: v.CoverGenerationID}
}
