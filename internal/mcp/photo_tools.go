package mcp

import (
	"context"
	"encoding/json/v2"
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
	Kind                  string          `json:"kind"`
	Revision              int64           `json:"revision"`
	ExcludedAt            *string         `json:"excluded_at,omitzero"`
	DisplayFileID         *string         `json:"display_file_id,omitzero"`
	DisplayOverrideFileID *string         `json:"display_override_file_id,omitzero"`
	DisplaySource         string          `json:"display_source"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	Agreement             map[string]bool `json:"agreement"`
	Files                 []api.PhotoFile `json:"files"`
	TotalFiles            int             `json:"total_files"`
	FileOffset            int             `json:"file_offset"`
	NextFileOffset        *int            `json:"next_file_offset,omitzero"`
}

func photoWriteTool(name string) bool {
	switch name {
	case "create_photo_asset", "attach_photo_file", "detach_photo_file", "exclude_photo_asset", "promote_photo_asset":
		return true
	default:
		return false
	}
}

func getPhotoAsset(ctx context.Context, lease *daemonLease, raw []byte) (photoAssetToolOutput, error) {
	var input struct {
		AssetID    string `json:"asset_id"`
		NodeID     int64  `json:"node_id"`
		FileOffset int    `json:"file_offset"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return photoAssetToolOutput{}, err
	}
	if (input.AssetID == "") == (input.NodeID == 0) {
		return photoAssetToolOutput{}, invalidToolArgumentsError()
	}
	asset, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PhotoAsset, error) {
		if input.AssetID != "" {
			return c.PhotoAsset(ctx, input.AssetID)
		}
		return c.PhotoAssetForNode(ctx, input.NodeID)
	})
	if err != nil {
		return photoAssetToolOutput{}, err
	}
	return photoAssetOutput(asset, input.FileOffset)
}

func photoAssetOutput(asset api.PhotoAsset, offset int) (photoAssetToolOutput, error) {
	if offset < 0 || offset > 256 {
		return photoAssetToolOutput{}, invalidToolArgumentsError()
	}
	output := photoAssetToolOutput{
		privateCache:          newPrivateCache(),
		ID:                    asset.ID,
		Kind:                  asset.Kind,
		Revision:              asset.Revision,
		ExcludedAt:            asset.ExcludedAt,
		DisplayFileID:         asset.DisplayFileID,
		DisplayOverrideFileID: asset.DisplayOverrideFileID,
		DisplaySource:         asset.DisplaySource,
		CreatedAt:             asset.CreatedAt,
		UpdatedAt:             asset.UpdatedAt,
		Files:                 []api.PhotoFile{},
		TotalFiles:            len(asset.Files),
		FileOffset:            offset,
		Agreement:             asset.Agreement,
	}
	// Each file appears in structured JSON and escaped text, with space left for the envelope.
	const fileBudget = maxToolResponseBytes/3 - 8<<10
	bytes := 0
	for _, file := range asset.Files[min(offset, len(asset.Files)):] {
		encoded, err := json.Marshal(file)
		if err != nil {
			return photoAssetToolOutput{}, err
		}
		if bytes+len(encoded)+1 > fileBudget {
			if len(output.Files) == 0 {
				return photoAssetToolOutput{}, errToolResultTooLarge
			}
			output.NextFileOffset = new(offset + len(output.Files))
			break
		}
		output.Files = append(output.Files, file)
		bytes += len(encoded) + 1
	}
	return output, nil
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
			NodeID int64  `json:"node_id"`
			Kind   string `json:"kind"`
			Role   string `json:"role"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
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
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				return c.AttachPhotoFile(ctx, input.AssetID, input.Revision, input.NodeID, input.Role, input.SidecarOfFileID)
			})
		}
	case "detach_photo_file":
		var input struct {
			AssetID                string `json:"asset_id"`
			Revision               int64  `json:"revision"`
			FileID                 string `json:"file_id"`
			ClearDependentSidecars bool   `json:"clear_dependent_sidecars"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				return c.DetachPhotoFile(ctx, input.AssetID, input.Revision, input.FileID, input.ClearDependentSidecars)
			})
		}
	case "exclude_photo_asset":
		var input struct {
			AssetID  string `json:"asset_id"`
			Revision int64  `json:"revision"`
			Excluded bool   `json:"excluded"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				return c.ExcludePhotoAsset(ctx, input.AssetID, input.Revision, input.Excluded)
			})
		}
	case "promote_photo_asset":
		var input struct {
			NodeID   int64  `json:"node_id"`
			Revision *int64 `json:"revision,omitzero"`
			Kind     string `json:"kind"`
			Role     string `json:"role"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			output, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoAsset, error) {
				return c.PromotePhotoNode(ctx, input.NodeID, input.Revision, input.Role, input.Kind)
			})
		}
	default:
		return nil, errors.New("unknown Docbank photo write tool")
	}
	if err != nil {
		return nil, err
	}
	page, err := photoAssetOutput(output, 0)
	var result *sdkmcp.CallToolResult
	if err == nil {
		result, err = boundedToolSuccess(validator, page, nil)
	}
	if err != nil {
		return nil, sanitizedDaemonError(errProcessingOutcomeUnknown,
			fmt.Errorf("photo mutation response failed output validation: %w", err))
	}
	return result, nil
}
