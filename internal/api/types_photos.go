package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/docbank/internal/store"
)

// PhotoFile is the daemon representation of one ordinary file node in an
// asset. The node remains authoritative for bytes and content versions.
type PhotoFile struct {
	Confirmed store.PhotoAuthoredFields `json:"confirmed_fields,omitzero" maximum:"127" doc:"Confirmed fields bitmask: rating=1, flag=2, label=4, caption=8, creator=16, copyright=32, rotation=64. An absent key means no confirmed fields; zero is omitted. Empty text and zero values can be confirmed."`
	Revision  int64                     `json:"revision" minimum:"1"`
	Rating    int                       `json:"rating" minimum:"0" maximum:"5"`
	Flag      string                    `json:"flag" enum:",pick,reject"`
	Label     string                    `json:"label" enum:",red,yellow,green,blue,purple"`
	Caption   string                    `json:"caption" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Creator   string                    `json:"creator" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Copyright string                    `json:"copyright" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Rotation  int                       `json:"rotation" enum:"0,90,180,270" doc:"Clockwise degrees on EXIF-normalized pixels: 0, 90, 180, or 270."`

	ID          string  `json:"id" format:"uuid"`
	AssetID     string  `json:"asset_id" format:"uuid"`
	NodeID      int64   `json:"node_id" minimum:"1"`
	Role        string  `json:"role" enum:"raw,image,video,sidecar"`
	SidecarOfID *string `json:"sidecar_of_file_id,omitzero" format:"uuid"`
	CreatedAt   string  `json:"created_at" format:"date-time"`
}

type PhotoAsset struct {
	Agreement             map[string]bool `json:"agreement"`
	ID                    string          `json:"id" format:"uuid"`
	Kind                  string          `json:"kind" enum:"photo,video"`
	Revision              int64           `json:"revision" minimum:"1"`
	HiddenAt              *string         `json:"hidden_at,omitzero" format:"date-time"`
	ExcludedAt            *string         `json:"excluded_at,omitzero" format:"date-time"`
	DisplayFileID         *string         `json:"display_file_id,omitzero" format:"uuid"`
	DisplayOverrideFileID *string         `json:"display_override_file_id,omitzero" format:"uuid"`
	DisplaySource         string          `json:"display_source" enum:"asset,vault,default,none"`
	CreatedAt             string          `json:"created_at" format:"date-time"`
	UpdatedAt             string          `json:"updated_at" format:"date-time"`
	Files                 []PhotoFile     `json:"files" maxItems:"256"`
}

type PhotoSettings struct {
	BurstGapSeconds int     `json:"burst_gap_seconds" minimum:"1" maximum:"30"`
	Preference      *string `json:"preference,omitzero" enum:"raw,image"`
	Revision        int64   `json:"revision" minimum:"1"`
	UpdatedAt       string  `json:"updated_at,omitzero" format:"date-time"`
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
	ResetPreference bool    `json:"reset_preference,omitzero" doc:"Reset the display preference, including when changing the burst gap. Conflicts with preference."`
	Preference      *string `json:"preference,omitzero" enum:"raw,image" doc:"Omit to keep the display preference when changing the burst gap, or reset it otherwise."`
	BurstGapSeconds *int    `json:"burst_gap_seconds,omitzero" minimum:"1" maximum:"30" doc:"Omit to keep the burst gap."`
	preferenceNull  bool
}

func (in *SetPhotoSettingsRequest) UnmarshalJSON(raw []byte) error {
	type fields SetPhotoSettingsRequest
	var decoded fields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	*in = SetPhotoSettingsRequest(decoded)
	in.preferenceNull = string(members["preference"]) == "null"
	return nil
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
	return PhotoFile{Confirmed: file.Confirmed, Revision: file.Revision, Rating: file.Rating, Flag: file.Flag, Label: file.Label, Caption: file.Caption, Creator: file.Creator, Copyright: file.Copyright, Rotation: file.Rotation, ID: file.ID, AssetID: file.AssetID, NodeID: file.NodeID, Role: file.Role, SidecarOfID: file.SidecarOfID, CreatedAt: file.CreatedAt}
}

func fromStorePhotoAsset(asset store.PhotoAsset) PhotoAsset {
	files := make([]PhotoFile, 0, len(asset.Files))
	for _, file := range asset.Files {
		files = append(files, fromStorePhotoFile(file))
	}
	return PhotoAsset{Agreement: asset.Agreement, ID: asset.ID, Kind: asset.Kind, Revision: asset.Revision, HiddenAt: asset.HiddenAt, ExcludedAt: asset.ExcludedAt, DisplayFileID: asset.DisplayFileID, DisplayOverrideFileID: asset.DisplayOverrideFileID, DisplaySource: asset.DisplaySource, CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt, Files: files}
}

func fromStorePhotoSettings(settings store.PhotoSettings) PhotoSettings {
	return PhotoSettings{BurstGapSeconds: settings.BurstGapSeconds, Preference: settings.Preference, Revision: settings.Revision, UpdatedAt: settings.UpdatedAt}
}

// GroupPhotoStacksRequest names photos to regroup. The store rejects repeated
// IDs: a schema uniqueItems check would echo the whole array once per repeat,
// so 250,000 copies of one ID could not be answered.
type GroupPhotoStacksRequest struct {
	Hidden   bool     `json:"hidden,omitempty"`
	AssetIDs []string `json:"asset_ids" maxItems:"250000" doc:"Distinct photo asset IDs, such as a saved Cull run."`
}

type GroupPhotoStacksResponse struct {
	GroupingIncomplete bool       `json:"grouping_incomplete,omitempty" doc:"Photos with stale source metadata group as photos with no serial until it updates, so their stacks can change."`
	BurstGapSeconds    int        `json:"burst_gap_seconds" minimum:"1" maximum:"30"`
	Groups             [][]string `json:"groups" doc:"Bursts of two or more submitted photos, in request order; other available photos are single."`
	UnavailableIDs     []string   `json:"unavailable_ids" doc:"Submitted photos that are no longer live in this library."`
}
