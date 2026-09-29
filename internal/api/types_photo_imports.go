package api

import (
	"encoding/json/v2"
	"time"

	"go.kenn.io/docbank/internal/store"
)

type PhotoImportStartRequest struct {
	SourceRoot  string `json:"source_root" minLength:"1" maxLength:"4096"`
	Destination string `json:"destination" minLength:"1" maxLength:"4096"`
}

// PhotoImportAmbiguity lists same-name files the import left unpaired.
type PhotoImportAmbiguity struct {
	Reason string                     `json:"reason" enum:"multiple_raw,separate_photos"`
	Files  []PhotoImportAmbiguousFile `json:"files"`
}

type PhotoImportAmbiguousFile struct {
	SourcePath string `json:"source_path,omitzero"`
	NodeID     int64  `json:"node_id" minimum:"1"`
	AssetID    string `json:"asset_id,omitzero" format:"uuid"`
	Role       string `json:"role"`
}

type PhotoImportRun struct {
	ID              string                 `json:"id" format:"uuid"`
	State           string                 `json:"state" enum:"queued,running,completed,failed,cancelled"`
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

// photoImportRequest decodes an operation's request, returning the zero
// request for a malformed row so listing never fails on one bad record.
func photoImportRequest(operation store.StorageOperation) store.PhotoImportRequest {
	var request store.PhotoImportRequest
	_ = json.Unmarshal([]byte(operation.RequestJSON), &request)
	return request
}

// fromStorePhotoImport projects a photo_import operation. A browser session
// sees no source paths or raw error text.
func fromStorePhotoImport(operation store.StorageOperation, browser bool) PhotoImportRun {
	request := photoImportRequest(operation)
	var receipt store.PhotoImportReceipt
	_ = json.Unmarshal([]byte(operation.ReceiptJSON), &receipt)
	out := PhotoImportRun{ID: operation.ID, State: string(operation.State),
		Destination: request.Destination, TotalGroups: operation.TotalObjects,
		CompletedGroups: operation.CompletedObjects, AddedGroups: receipt.Added,
		SkippedGroups: receipt.Skipped, FailedGroups: receipt.Failed,
		AmbiguousGroups: receipt.Ambiguous, CancelRequested: operation.CancelRequested,
		StartedAt: operation.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: operation.UpdatedAt.Format(time.RFC3339Nano)}
	if operation.FinishedAt != nil {
		out.FinishedAt = operation.FinishedAt.Format(time.RFC3339Nano)
	}
	if !browser {
		out.SourceRoot = request.SourceRoot
		out.Error = operation.Error
	}
	for _, ambiguity := range receipt.Ambiguities {
		item := PhotoImportAmbiguity{Reason: ambiguity.Reason, Files: make([]PhotoImportAmbiguousFile, 0, len(ambiguity.Files))}
		for _, file := range ambiguity.Files {
			entry := PhotoImportAmbiguousFile{NodeID: file.NodeID, AssetID: file.AssetID, Role: file.Role}
			if !browser {
				entry.SourcePath = file.SourcePath
			}
			item.Files = append(item.Files, entry)
		}
		out.Ambiguities = append(out.Ambiguities, item)
	}
	return out
}
