package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type photoAssetToolOutput struct {
	privateCache

	ID                    string          `json:"id"`
	OwnerID               *string         `json:"owner_id,omitzero"`
	HiddenAt              *string         `json:"hidden_at,omitzero"`
	Kind                  string          `json:"kind"`
	Revision              int64           `json:"revision"`
	ExcludedAt            *string         `json:"excluded_at,omitzero"`
	DisplayFileID         *string         `json:"display_file_id,omitzero"`
	DisplayOverrideFileID *string         `json:"display_override_file_id,omitzero"`
	DisplaySource         string          `json:"display_source"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	Files                 []api.PhotoFile `json:"files"`
}

type photoOwnerToolOutput struct {
	privateCache

	ID        string `json:"id"`
	Name      string `json:"name"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func photoWriteTool(name string) bool {
	switch name {
	case "create_photo_asset", "attach_photo_file", "detach_photo_file", "exclude_photo_asset", "promote_photo_asset":
		return true
	case "add_photo_owner", "rename_photo_owner", "remove_photo_owner":
		return true
	default:
		return false
	}
}

func getPhotoAsset(ctx context.Context, lease *daemonLease, raw []byte) (photoAssetToolOutput, error) {
	var input struct {
		AssetID string `json:"asset_id"`
		NodeID  int64  `json:"node_id"`
		OwnerID string `json:"owner_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return photoAssetToolOutput{}, err
	}
	if (input.AssetID == "") == (input.NodeID == 0) {
		return photoAssetToolOutput{}, invalidToolArgumentsError()
	}
	asset, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PhotoAsset, error) {
		if input.OwnerID != "" {
			c = c.WithPhotoOwner(input.OwnerID)
		}
		if input.AssetID != "" {
			return c.PhotoAsset(ctx, input.AssetID)
		}
		return c.PhotoAssetForNode(ctx, input.NodeID)
	})
	if err != nil {
		return photoAssetToolOutput{}, err
	}
	return photoAssetOutput(asset), nil
}

func photoAssetOutput(asset api.PhotoAsset) photoAssetToolOutput {
	return photoAssetToolOutput{
		privateCache:          newPrivateCache(),
		ID:                    asset.ID,
		OwnerID:               asset.OwnerID,
		HiddenAt:              asset.HiddenAt,
		Kind:                  asset.Kind,
		Revision:              asset.Revision,
		ExcludedAt:            asset.ExcludedAt,
		DisplayFileID:         asset.DisplayFileID,
		DisplayOverrideFileID: asset.DisplayOverrideFileID,
		DisplaySource:         asset.DisplaySource,
		CreatedAt:             asset.CreatedAt,
		UpdatedAt:             asset.UpdatedAt,
		Files:                 asset.Files,
	}
}

func photoWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := executePhotoWriteTool(ctx, lease, name, validator, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return output, nil
	}
}

func executePhotoWriteTool(
	ctx context.Context, lease *daemonLease, name string, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	var output api.PhotoAsset
	var err error
	switch name {
	case "create_photo_asset":
		var input struct {
			NodeID  int64  `json:"node_id"`
			Kind    string `json:"kind"`
			Role    string `json:"role"`
			OwnerID string `json:"owner_id"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				if input.OwnerID != "" {
					c = c.WithPhotoOwner(input.OwnerID)
				}
				return c.CreatePhotoAsset(ctx, input.NodeID, input.Role, input.Kind)
			})
		}
	case "attach_photo_file":
		var input struct {
			AssetID         string  `json:"asset_id"`
			Revision        int64   `json:"revision"`
			NodeID          int64   `json:"node_id"`
			Role            string  `json:"role"`
			SidecarOfFileID *string `json:"sidecar_of_file_id,omitzero"`
			OwnerID         string  `json:"owner_id"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				if input.OwnerID != "" {
					c = c.WithPhotoOwner(input.OwnerID)
				}
				return c.AttachPhotoFile(ctx, input.AssetID, input.Revision, input.NodeID, input.Role, input.SidecarOfFileID)
			})
		}
	case "detach_photo_file":
		var input struct {
			AssetID                string `json:"asset_id"`
			Revision               int64  `json:"revision"`
			FileID                 string `json:"file_id"`
			ClearDependentSidecars bool   `json:"clear_dependent_sidecars"`
			OwnerID                string `json:"owner_id"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				if input.OwnerID != "" {
					c = c.WithPhotoOwner(input.OwnerID)
				}
				return c.DetachPhotoFile(ctx, input.AssetID, input.Revision, input.FileID, input.ClearDependentSidecars)
			})
		}
	case "exclude_photo_asset":
		var input struct {
			AssetID  string `json:"asset_id"`
			Revision int64  `json:"revision"`
			Excluded bool   `json:"excluded"`
			OwnerID  string `json:"owner_id"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				if input.OwnerID != "" {
					c = c.WithPhotoOwner(input.OwnerID)
				}
				return c.ExcludePhotoAsset(ctx, input.AssetID, input.Revision, input.Excluded)
			})
		}
	case "promote_photo_asset":
		var input struct {
			NodeID   int64  `json:"node_id"`
			Revision *int64 `json:"revision,omitzero"`
			Kind     string `json:"kind"`
			Role     string `json:"role"`
			OwnerID  string `json:"owner_id"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				if input.OwnerID != "" {
					c = c.WithPhotoOwner(input.OwnerID)
				}
				return c.PromotePhotoNode(ctx, input.NodeID, input.Revision, input.Role, input.Kind)
			})
		}
	default:
		return nil, errors.New("unknown Docbank photo write tool")
	}
	if err != nil {
		return nil, err
	}
	result, err := boundedToolSuccess(validator, photoAssetOutput(output), nil)
	if err != nil {
		return nil, sanitizedDaemonError(errProcessingOutcomeUnknown,
			fmt.Errorf("photo mutation response failed output validation: %w", err))
	}
	return result, nil
}

func photoOwnerToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executePhotoOwnerTool(ctx, lease, name, validator, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return result, nil
	}
}

func executePhotoOwnerTool(
	ctx context.Context, lease *daemonLease, name string, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	var input struct {
		Name     string `json:"name"`
		OwnerID  string `json:"owner_id"`
		Revision int64  `json:"revision"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	var output any
	switch name {
	case "list_photo_owners":
		owners, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) ([]api.PhotoOwner, error) {
			return c.PhotoOwners(ctx)
		})
		if err != nil {
			return nil, err
		}
		items := make([]photoOwnerToolOutput, len(owners))
		for i, owner := range owners {
			items[i] = photoOwnerOutput(owner)
		}
		output = struct {
			privateCache

			Owners []photoOwnerToolOutput `json:"owners"`
		}{privateCache: newPrivateCache(), Owners: items}
	case "add_photo_owner":
		owner, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoOwner, error) {
			return c.CreatePhotoOwner(ctx, input.Name)
		})
		if err != nil {
			return nil, err
		}
		output = photoOwnerOutput(owner)
	case "rename_photo_owner":
		owner, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoOwner, error) {
			return c.RenamePhotoOwner(ctx, input.OwnerID, input.Revision, input.Name)
		})
		if err != nil {
			return nil, err
		}
		output = photoOwnerOutput(owner)
	case "remove_photo_owner":
		err := daemonProcessingStartVoid(ctx, lease, func(c *daemonconn.Connection) error {
			return c.RemovePhotoOwner(ctx, input.OwnerID, input.Revision)
		})
		if err != nil {
			return nil, err
		}
		output = struct {
			privateCache

			ID string `json:"id"`
		}{privateCache: newPrivateCache(), ID: input.OwnerID}
	default:
		return nil, errors.New("unknown Docbank photo owner tool")
	}
	return boundedToolSuccess(validator, output, nil)
}

func photoOwnerOutput(owner api.PhotoOwner) photoOwnerToolOutput {
	return photoOwnerToolOutput{privateCache: newPrivateCache(), ID: owner.ID, Name: owner.Name,
		Revision: owner.Revision, CreatedAt: owner.CreatedAt, UpdatedAt: owner.UpdatedAt}
}
