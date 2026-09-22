package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
)

type exportLoadFilePackageInput struct {
	SnapshotID        string `json:"snapshot_id"`
	SourcePackageID   string `json:"source_package_id"`
	BatesAllocationID string `json:"bates_allocation_id"`
	ProfileID         string `json:"profile_id"`
	DestinationPath   string `json:"destination_path"`
	Overwrite         bool   `json:"overwrite"`
}

type exportLoadFilePackageOutput struct {
	privateCache

	SnapshotID        string `json:"snapshot_id"`
	SourcePackageID   string `json:"source_package_id,omitzero"`
	BatesAllocationID string `json:"bates_allocation_id,omitzero"`
	ProfileID         string `json:"profile_id"`
	DestinationPath   string `json:"destination_path"`
	ArchiveSHA256     string `json:"archive_sha256"`
	ManifestSHA256    string `json:"manifest_sha256"`
	CrosswalkSHA256   string `json:"crosswalk_sha256"`
	Size              int64  `json:"size"`
	Records           int    `json:"records"`
	Pages             int    `json:"pages"`
	State             string `json:"state"`
}

func exportLoadFilePackage(ctx context.Context, lease *daemonLease, raw []byte) (exportLoadFilePackageOutput, error) {
	var input exportLoadFilePackageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportLoadFilePackageOutput{}, err
	}
	if _, err := uuid.Parse(input.SnapshotID); err != nil ||
		(input.SourcePackageID != "" && !validToolUUID(input.SourcePackageID)) ||
		(input.BatesAllocationID != "" && !validToolUUID(input.BatesAllocationID)) ||
		!filepath.IsAbs(input.DestinationPath) || filepath.Clean(input.DestinationPath) != input.DestinationPath {
		return exportLoadFilePackageOutput{}, invalidToolArgumentsError()
	}
	parent := filepath.Dir(input.DestinationPath)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return exportLoadFilePackageOutput{}, invalidToolArgumentsError()
	}
	if destination, err := os.Lstat(input.DestinationPath); err == nil {
		if destination.Mode()&os.ModeSymlink != 0 || !destination.Mode().IsRegular() {
			return exportLoadFilePackageOutput{}, invalidToolArgumentsError()
		}
		if !input.Overwrite {
			return exportLoadFilePackageOutput{}, os.ErrExist
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return exportLoadFilePackageOutput{}, err
	}
	stage, err := filepublish.CreateStage(parent, ".docbank-loadfile-")
	if err != nil {
		return exportLoadFilePackageOutput{}, err
	}
	staged, stagedPath := stage.File, stage.Path()
	keep := false
	defer func() {
		if !keep {
			_ = stage.Cleanup()
		}
	}()
	request := api.PackageExportRequest{SnapshotID: input.SnapshotID, SourcePackageID: input.SourcePackageID,
		BatesAllocationID: input.BatesAllocationID, ProfileID: input.ProfileID}
	receipt, err := daemonRead(ctx, lease, func(ctx context.Context, connection *daemonconn.Connection) (*api.PackageExportTicket, error) {
		value, err := connection.CreatePackageExportTo(ctx, request, staged)
		return &value, err
	})
	if err != nil {
		return exportLoadFilePackageOutput{}, err
	}
	if err := staged.Sync(); err != nil {
		return exportLoadFilePackageOutput{}, err
	}
	if err := staged.Close(); err != nil {
		return exportLoadFilePackageOutput{}, err
	}
	published, publishErr := filepublish.Publish(stagedPath, input.DestinationPath, input.Overwrite)
	if !published {
		return exportLoadFilePackageOutput{}, publishErr
	}
	_ = os.Remove(stagedPath)
	keep = true
	_ = stage.Cleanup()
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
	}
	return exportLoadFilePackageOutput{privateCache: newPrivateCache(), SnapshotID: receipt.SnapshotID,
		SourcePackageID: receipt.SourcePackageID, BatesAllocationID: receipt.BatesAllocationID,
		ProfileID: receipt.ProfileID, DestinationPath: input.DestinationPath,
		ArchiveSHA256: receipt.ArchiveSHA256, ManifestSHA256: receipt.ManifestSHA256,
		CrosswalkSHA256: receipt.CrosswalkSHA256, Size: receipt.Size, Records: receipt.Records,
		Pages: receipt.Pages, State: state}, nil
}

func validToolUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

func packageExportToolHandler(
	lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := exportLoadFilePackage(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, exportLoadFilePackageToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
