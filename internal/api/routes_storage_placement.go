package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func registerStoragePlacementRoutes(api huma.API, d Deps, g *gate) {
	previews := newPlacementPreviewRegistry()
	type previewOutput struct{ Body StoragePlacementPreview }
	huma.Register(api, huma.Operation{
		OperationID: "previewStoragePlacement", Method: http.MethodPost,
		Path:    "/api/v1/storage/place/preview",
		Summary: "Preview verified placement for retained content beneath one node",
	}, func(ctx context.Context, in *struct {
		Body struct {
			NodeID                 int64  `json:"node_id" minimum:"1"`
			Source                 string `json:"source,omitempty"`
			Destination            string `json:"destination" minLength:"1"`
			RetireSource           bool   `json:"retire_source,omitempty"`
			AllowAuditedRemoteOnly bool   `json:"allow_audited_remote_only,omitempty"`
		}
	}) (*previewOutput, error) {
		source, destination, err := placementStores(ctx, d, in.Body.Source, in.Body.Destination)
		if err != nil {
			return nil, err
		}
		plan, err := d.Store.PlanPlacement(ctx, store.PlacementRequest{
			TargetNodeID: in.Body.NodeID, SourceStoreID: source.ID,
			DestinationStoreID: destination.ID, RetireSource: in.Body.RetireSource,
			AllowAuditedRemoteOnly: in.Body.AllowAuditedRemoteOnly,
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		token, expiresAt, err := previews.issue("place", plan)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", err.Error())
		}
		return &previewOutput{Body: placementPreviewAPI(plan, token, expiresAt)}, nil
	})

	type operationOutput struct{ Body StorageOperation }
	huma.Register(api, huma.Operation{
		OperationID: "startStoragePlacement", Method: http.MethodPost,
		Path:    "/api/v1/storage/place",
		Summary: "Start the exact verified placement reviewed by a preview",
	}, func(ctx context.Context, in *struct {
		Body struct {
			PreviewToken string `json:"preview_token" minLength:"43" maxLength:"43"`
		}
	}) (*operationOutput, error) {
		plan, err := previews.take("place", in.Body.PreviewToken)
		if err != nil {
			return nil, NewError(http.StatusConflict, "storage_preview_stale", err.Error())
		}
		operation, err := createPlacementOperation(ctx, d.Store, "place", plan)
		if err != nil {
			return nil, err
		}
		runner := blob.PlacementRunner{
			Metadata: d.Store, Blobs: d.Blobs, Commit: g.PhysicalMutate,
		}
		if err := runner.Start(d.Jobs, operation.ID); err != nil {
			return nil, NewError(http.StatusServiceUnavailable,
				"storage_job_unavailable",
				fmt.Sprintf("placement is queued but could not start: %v", err))
		}
		return &operationOutput{Body: storageOperationAPI(operation)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "previewStorageEvacuation", Method: http.MethodPost,
		Path:    "/api/v1/storage/evacuate/preview",
		Summary: "Preview complete evacuation of one secondary store to primary",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Store string `json:"store" minLength:"1"`
		}
	}) (*previewOutput, error) {
		source, err := d.Store.BlobStoreBySelector(ctx, in.Body.Store)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if source.Role == "primary" {
			return nil, FromStoreError(store.ErrBlobStorePrimary)
		}
		primary, err := d.Store.PrimaryBlobStore(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		plan, err := d.Store.PlanPlacement(ctx, store.PlacementRequest{
			TargetNodeID: d.Store.RootID(), SourceStoreID: source.ID,
			DestinationStoreID: primary.ID, RetireSource: true, Evacuate: true,
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		token, expiresAt, err := previews.issue("evacuate", plan)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", err.Error())
		}
		return &previewOutput{Body: placementPreviewAPI(plan, token, expiresAt)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "startStorageEvacuation", Method: http.MethodPost,
		Path:    "/api/v1/storage/evacuate",
		Summary: "Start the exact store evacuation reviewed by a preview",
	}, func(ctx context.Context, in *struct {
		Body struct {
			PreviewToken string `json:"preview_token" minLength:"43" maxLength:"43"`
		}
	}) (*operationOutput, error) {
		plan, err := previews.take("evacuate", in.Body.PreviewToken)
		if err != nil {
			return nil, NewError(http.StatusConflict, "storage_preview_stale", err.Error())
		}
		if err := d.Store.BeginBlobStoreEvacuation(ctx, plan.Request.SourceStoreID); err != nil {
			return nil, FromStoreError(err)
		}
		operation, err := createPlacementOperation(ctx, d.Store, "evacuate", plan)
		if err != nil {
			return nil, err
		}
		runner := blob.PlacementRunner{
			Metadata: d.Store, Blobs: d.Blobs, Commit: g.PhysicalMutate,
		}
		if err := runner.Start(d.Jobs, operation.ID); err != nil {
			return nil, NewError(http.StatusServiceUnavailable,
				"storage_job_unavailable",
				fmt.Sprintf("evacuation is queued but could not start: %v", err))
		}
		return &operationOutput{Body: storageOperationAPI(operation)}, nil
	})
	registerStorageRecoveryRoutes(api, d, g, previews, "repair")
	registerStorageRecoveryRoutes(api, d, g, previews, "salvage")
}

func registerStorageRecoveryRoutes(
	api huma.API, d Deps, g *gate, previews *placementPreviewRegistry, kind string,
) {
	type previewOutput struct{ Body StorageRecoveryPreview }
	huma.Register(api, huma.Operation{
		OperationID: "previewStorage" + titleWord(kind),
		Method:      http.MethodPost,
		Path:        "/api/v1/storage/" + kind + "/preview",
		Summary:     "Preview one explicit verified storage " + kind,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Hash  string `json:"hash" minLength:"64" maxLength:"64"`
			Store string `json:"store" minLength:"1"`
		}
	}) (*previewOutput, error) {
		plan, err := d.Store.PlanStorageRecovery(
			ctx, kind, in.Body.Hash, in.Body.Store,
		)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if kind == "salvage" {
			observation := d.Blobs.RefreshStore(ctx, string(plan.Sources[0].StoreID))
			if observation.State != blob.StoreFenced {
				return nil, NewError(
					http.StatusConflict, "store_not_fenced",
					"salvage requires a fresh fenced-store observation",
				)
			}
		}
		sourceStoreIDs := make([]string, 0, len(plan.Sources))
		for _, source := range plan.Sources {
			sourceStoreIDs = append(sourceStoreIDs, string(source.StoreID))
		}
		token, expiresAt, err := previews.issueRecovery(plan)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", err.Error())
		}
		return &previewOutput{Body: StorageRecoveryPreview{
			Kind: kind, PlanDigest: plan.Digest, Hash: plan.Hash, Bytes: plan.Size,
			SourceStoreIDs:     sourceStoreIDs,
			DestinationStoreID: plan.Destination,
			PreviewToken:       token, ExpiresAt: expiresAt.Format(time.RFC3339Nano),
		}}, nil
	})

	type operationOutput struct{ Body StorageOperation }
	huma.Register(api, huma.Operation{
		OperationID: "startStorage" + titleWord(kind),
		Method:      http.MethodPost,
		Path:        "/api/v1/storage/" + kind,
		Summary:     "Start the exact storage " + kind + " reviewed by a preview",
	}, func(ctx context.Context, in *struct {
		Body struct {
			PreviewToken string `json:"preview_token" minLength:"43" maxLength:"43"`
		}
	}) (*operationOutput, error) {
		plan, err := previews.takeRecovery(kind, in.Body.PreviewToken)
		if err != nil {
			return nil, NewError(http.StatusConflict, "storage_preview_stale", err.Error())
		}
		operation, err := createRecoveryOperation(ctx, d.Store, plan)
		if err != nil {
			return nil, err
		}
		runner := blob.PlacementRunner{
			Metadata: d.Store, Blobs: d.Blobs, Commit: g.PhysicalMutate,
		}
		if err := runner.Start(d.Jobs, operation.ID); err != nil {
			return nil, NewError(http.StatusServiceUnavailable,
				"storage_job_unavailable",
				fmt.Sprintf("%s is queued but could not start: %v", kind, err))
		}
		return &operationOutput{Body: storageOperationAPI(operation)}, nil
	})
}

func createRecoveryOperation(
	ctx context.Context, metadata *store.Store, plan store.StorageRecoveryPlan,
) (store.StorageOperation, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return store.StorageOperation{}, NewError(
			http.StatusInternalServerError, "internal", err.Error(),
		)
	}
	storeReferences := make([]store.StorageOperationStoreReference, 0, len(plan.Sources)+1)
	for _, source := range plan.Sources {
		storeReferences = append(storeReferences, store.StorageOperationStoreReference{
			StoreID: string(source.StoreID), Role: "source",
		})
	}
	storeReferences = append(storeReferences, store.StorageOperationStoreReference{
		StoreID: plan.Destination, Role: "destination",
	})
	operation, err := metadata.CreateStorageOperation(ctx, store.StorageOperationCreate{
		Kind: plan.Kind, RequestDigest: plan.Digest,
		StoreReferences: storeReferences, RequestJSON: string(encoded),
		PlanJSON: string(encoded), TotalObjects: 1,
	})
	if err != nil {
		return store.StorageOperation{}, FromStoreError(err)
	}
	return operation, nil
}

func titleWord(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func createPlacementOperation(
	ctx context.Context, metadata *store.Store, kind string, plan store.PlacementPlan,
) (store.StorageOperation, error) {
	requestJSON, err := json.Marshal(plan.Request)
	if err != nil {
		return store.StorageOperation{}, NewError(
			http.StatusInternalServerError, "internal", err.Error(),
		)
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return store.StorageOperation{}, NewError(
			http.StatusInternalServerError, "internal", err.Error(),
		)
	}
	operation, err := metadata.CreateStorageOperation(ctx, store.StorageOperationCreate{
		Kind: kind, RequestDigest: plan.Digest,
		SourceStoreID: func() string {
			if kind == "evacuate" {
				return plan.Request.SourceStoreID
			}
			return ""
		}(),
		StoreReferences: []store.StorageOperationStoreReference{
			{StoreID: plan.Request.SourceStoreID, Role: "source"},
			{StoreID: plan.Request.DestinationStoreID, Role: "destination"},
		},
		RequestJSON: string(requestJSON), PlanJSON: string(planJSON),
		TotalObjects: int64(len(plan.Hashes)),
	})
	if err != nil {
		return store.StorageOperation{}, FromStoreError(err)
	}
	return operation, nil
}

func placementStores(
	ctx context.Context, d Deps, sourceSelector, destinationSelector string,
) (store.BlobStore, store.BlobStore, error) {
	if sourceSelector == "" {
		sourceSelector = d.Store.PrimaryBlobStoreID()
	}
	source, err := d.Store.BlobStoreBySelector(ctx, sourceSelector)
	if err != nil {
		return store.BlobStore{}, store.BlobStore{}, FromStoreError(err)
	}
	destination, err := d.Store.BlobStoreBySelector(ctx, destinationSelector)
	if err != nil {
		return store.BlobStore{}, store.BlobStore{}, FromStoreError(err)
	}
	if source.ID == destination.ID {
		return store.BlobStore{}, store.BlobStore{}, NewError(
			http.StatusUnprocessableEntity, "validation",
			"source and destination stores must differ",
		)
	}
	return source, destination, nil
}

func placementPreviewAPI(
	plan store.PlacementPlan, token string, expiresAt time.Time,
) StoragePlacementPreview {
	return StoragePlacementPreview{
		PlanDigest: plan.Digest, TargetNodeID: plan.Request.TargetNodeID,
		SourceStoreID:      plan.Request.SourceStoreID,
		DestinationStoreID: plan.Request.DestinationStoreID,
		Objects:            int64(len(plan.Hashes)), Versions: plan.SelectedVersions,
		LogicalBytes: plan.LogicalBytes, TransferBytes: plan.TransferBytes,
		ReadBackBytes:       plan.ReadBackBytes,
		RemoteEgressBytes:   plan.RemoteEgressBytes,
		ScratchBytes:        plan.ScratchBytes,
		AlreadyPresentBytes: plan.AlreadyPresentBytes,
		RetirableBytes:      plan.RetirableBytes, SharedBytes: plan.SharedBytes,
		AuditPinnedBytes: plan.AuditPinnedBytes, PackBlockedBytes: plan.PackBlockedBytes,
		PreviewToken: token, ExpiresAt: expiresAt.Format(time.RFC3339Nano),
	}
}

func storageOperationAPI(operation store.StorageOperation) StorageOperation {
	result := StorageOperation{
		ID: operation.ID, Kind: operation.Kind, State: string(operation.State),
		PlanDigest: operation.RequestDigest, TotalObjects: operation.TotalObjects,
		CompletedObjects: operation.CompletedObjects,
		CopiedObjects:    operation.CopiedObjects, CopiedBytes: operation.CopiedBytes,
		CancelRequested: operation.CancelRequested, Error: operation.Error,
		CreatedAt: operation.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt: operation.UpdatedAt.Format(time.RFC3339Nano),
	}
	if operation.ReceiptJSON != "" {
		var receipt any
		if json.Unmarshal([]byte(operation.ReceiptJSON), &receipt) == nil {
			result.Receipt = receipt
		}
	}
	if operation.FinishedAt != nil {
		result.FinishedAt = operation.FinishedAt.Format(time.RFC3339Nano)
	}
	return result
}
