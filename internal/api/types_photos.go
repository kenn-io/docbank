package api

import "go.kenn.io/docbank/internal/store"

// PhotoFile is the daemon representation of one ordinary file node in an
// asset. The node remains authoritative for bytes and content versions.
type PhotoFile struct {
	ID          string  `json:"id" format:"uuid"`
	AssetID     string  `json:"asset_id" format:"uuid"`
	NodeID      int64   `json:"node_id" minimum:"1"`
	Role        string  `json:"role" enum:"raw,image,video,sidecar"`
	SidecarOfID *string `json:"sidecar_of_file_id,omitzero" format:"uuid"`
	CreatedAt   string  `json:"created_at" format:"date-time"`
}

type PhotoAsset struct {
	ID                    string      `json:"id" format:"uuid"`
	Kind                  string      `json:"kind" enum:"photo,video"`
	Revision              int64       `json:"revision" minimum:"1"`
	ExcludedAt            *string     `json:"excluded_at,omitzero" format:"date-time"`
	DisplayFileID         *string     `json:"display_file_id,omitzero" format:"uuid"`
	DisplayOverrideFileID *string     `json:"display_override_file_id,omitzero" format:"uuid"`
	DisplaySource         string      `json:"display_source" enum:"asset,vault,default,none"`
	CreatedAt             string      `json:"created_at" format:"date-time"`
	UpdatedAt             string      `json:"updated_at" format:"date-time"`
	Files                 []PhotoFile `json:"files" maxItems:"256"`
}

type PhotoSettings struct {
	Preference *string `json:"preference,omitzero" enum:"raw,image"`
	Revision   int64   `json:"revision" minimum:"1"`
	UpdatedAt  string  `json:"updated_at,omitzero" format:"date-time"`
}

type CreatePhotoAssetRequest struct {
	NodeID int64  `json:"node_id" minimum:"1"`
	Kind   string `json:"kind,omitzero" enum:"photo,video"`
	Role   string `json:"role,omitzero" enum:"raw,image,video,sidecar"`
}

type AttachPhotoFileRequest struct {
	NodeID      int64   `json:"node_id" minimum:"1"`
	Role        string  `json:"role,omitzero" enum:"raw,image,video,sidecar"`
	SidecarOfID *string `json:"sidecar_of_file_id,omitzero" format:"uuid"`
}

type SetPhotoDisplayRequest struct {
	FileID *string `json:"file_id,omitzero" format:"uuid"`
}

type SetPhotoExcludedRequest struct {
	Excluded bool `json:"excluded"`
}

type SetPhotoSettingsRequest struct {
	Preference *string `json:"preference,omitzero" enum:"raw,image"`
}

type photoAssetOutput struct {
	ETag string `header:"ETag"`
	Body PhotoAsset
}

type photoSettingsOutput struct {
	ETag string `header:"ETag"`
	Body PhotoSettings
}

func fromStorePhotoFile(file store.PhotoFile) PhotoFile {
	return PhotoFile{ID: file.ID, AssetID: file.AssetID, NodeID: file.NodeID, Role: file.Role, SidecarOfID: file.SidecarOfID, CreatedAt: file.CreatedAt}
}

func fromStorePhotoAsset(asset store.PhotoAsset) PhotoAsset {
	files := make([]PhotoFile, 0, len(asset.Files))
	for _, file := range asset.Files {
		files = append(files, fromStorePhotoFile(file))
	}
	return PhotoAsset{ID: asset.ID, Kind: asset.Kind, Revision: asset.Revision, ExcludedAt: asset.ExcludedAt, DisplayFileID: asset.DisplayFileID, DisplayOverrideFileID: asset.DisplayOverrideFileID, DisplaySource: asset.DisplaySource, CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt, Files: files}
}

func fromStorePhotoSettings(settings store.PhotoSettings) PhotoSettings {
	return PhotoSettings{Preference: settings.Preference, Revision: settings.Revision, UpdatedAt: settings.UpdatedAt}
}
