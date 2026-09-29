package api

import "go.kenn.io/docbank/internal/store"

type PhotoImportStartRequest struct {
	SourceRoot  string             `json:"source_root" minLength:"1" maxLength:"4096"`
	Destination string             `json:"destination" minLength:"1" maxLength:"4096"`
	Choice      *PhotoImportChoice `json:"choice,omitzero"`
}

type PhotoImportChoice struct {
	GroupKey      string `json:"group_key,omitzero"`
	RawAssetID    string `json:"raw_asset_id,omitzero" format:"uuid"`
	RawFileID     string `json:"raw_file_id,omitzero" format:"uuid"`
	AssetRevision int64  `json:"asset_revision,omitzero" minimum:"1"`
	RawSourcePath string `json:"raw_source_path,omitzero" maxLength:"4096"`
	RawBlobHash   string `json:"raw_blob_hash,omitzero" pattern:"^[0-9a-f]{64}$"`
}

type PhotoImportCandidate struct {
	AssetID    string `json:"asset_id,omitzero" format:"uuid"`
	FileID     string `json:"file_id,omitzero" format:"uuid"`
	NodeID     int64  `json:"node_id,omitzero" minimum:"1"`
	Revision   int64  `json:"revision,omitzero" minimum:"1"`
	SourcePath string `json:"source_path,omitzero"`
	BlobHash   string `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
}

type PhotoImportAmbiguity struct {
	GroupKey   string                 `json:"group_key"`
	Candidates []PhotoImportCandidate `json:"candidates"`
}

type PhotoImportRun struct {
	ID              string                 `json:"id" format:"uuid"`
	Revision        int64                  `json:"revision" minimum:"1"`
	State           string                 `json:"state"`
	SourceRoot      string                 `json:"source_root,omitzero"`
	Destination     string                 `json:"destination"`
	TotalGroups     int64                  `json:"total_groups" minimum:"0"`
	CompletedGroups int64                  `json:"completed_groups" minimum:"0"`
	AddedGroups     int64                  `json:"added_groups" minimum:"0"`
	SkippedGroups   int64                  `json:"skipped_groups" minimum:"0"`
	FailedGroups    int64                  `json:"failed_groups" minimum:"0"`
	AmbiguousGroups int64                  `json:"ambiguous_groups" minimum:"0"`
	CancelRequested bool                   `json:"cancel_requested"`
	Error           string                 `json:"error,omitzero"`
	Ambiguities     []PhotoImportAmbiguity `json:"ambiguities,omitzero"`
	StartedAt       string                 `json:"started_at"`
	UpdatedAt       string                 `json:"updated_at"`
	FinishedAt      string                 `json:"finished_at,omitzero"`
}

type PhotoImportRunList struct {
	Items []PhotoImportRun `json:"items"`
}

func fromStorePhotoImportChoice(choice *PhotoImportChoice) *store.PhotoImportChoice {
	if choice == nil {
		return nil
	}
	return &store.PhotoImportChoice{GroupKey: choice.GroupKey, RawAssetID: choice.RawAssetID,
		RawFileID: choice.RawFileID, AssetRevision: choice.AssetRevision,
		RawSourcePath: choice.RawSourcePath, RawBlobHash: choice.RawBlobHash}
}

func fromStorePhotoImportRun(run store.PhotoImportRun, browser bool) PhotoImportRun {
	out := PhotoImportRun{ID: run.ID, Revision: run.Revision, State: run.State,
		Destination: run.Destination, TotalGroups: run.TotalGroups,
		CompletedGroups: run.CompletedGroups, AddedGroups: run.AddedGroups,
		SkippedGroups: run.SkippedGroups, FailedGroups: run.FailedGroups,
		AmbiguousGroups: run.AmbiguousGroups, CancelRequested: run.CancelRequested,
		StartedAt: run.StartedAt, UpdatedAt: run.UpdatedAt, FinishedAt: run.FinishedAt}
	if !browser {
		out.SourceRoot = run.SourceRoot
		out.Error = run.Error
	}
	for _, ambiguity := range run.Ambiguities {
		item := PhotoImportAmbiguity{GroupKey: ambiguity.GroupKey}
		if browser {
			item.GroupKey = ""
		}
		for _, candidate := range ambiguity.Candidates {
			sourcePath := candidate.SourcePath
			if browser {
				sourcePath = ""
			}
			item.Candidates = append(item.Candidates, PhotoImportCandidate{AssetID: candidate.AssetID,
				FileID: candidate.FileID, NodeID: candidate.NodeID, Revision: candidate.Revision,
				SourcePath: sourcePath, BlobHash: candidate.BlobHash})
		}
		out.Ambiguities = append(out.Ambiguities, item)
	}
	return out
}
