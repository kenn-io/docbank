package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

func registerInfoRoute(api huma.API, d Deps) {
	type infoOutput struct{ Body VaultInfo }
	huma.Register(api, huma.Operation{
		OperationID: "vaultInfo", Method: http.MethodGet, Path: "/api/v1/info",
		Summary: "Identify the selected vault and summarize its contents",
	}, func(ctx context.Context, _ *struct{}) (*infoOutput, error) {
		logical, err := d.Store.Info(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		physical, err := d.Blobs.Stats(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &infoOutput{Body: VaultInfo{
			VaultID: logical.VaultID, VaultPath: d.VaultRoot,
			LiveFiles: logical.LiveFiles, LiveDirectories: logical.LiveDirectories,
			TrashedNodes: logical.TrashedNodes, ContentVersions: logical.ContentVersions,
			LogicalVersionBytes: logical.LogicalVersionBytes,
			TrackedBlobs:        logical.TrackedBlobs, TrackedBlobBytes: logical.TrackedBlobBytes,
			Storage: StorageStatus{
				LooseBlobs: physical.LooseBlobs, LooseBytes: physical.LooseBytes,
				Packs: physical.Packs, PackStoredBytes: physical.PackStoredBytes,
				PackedBlobs: physical.PackedBlobs, PackedRawBytes: physical.PackedRawBytes,
				PackedStoredBytes: physical.PackedStoredBytes,
				DeadPackedBytes:   physical.DeadPackedBytes,
			},
		}}, nil
	})
}
