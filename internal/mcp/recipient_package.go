package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

const recipientPackageLocalIOCode = "recipient_package_local_io"
const recipientPackageLimitCode = "recipient_package_limit"

var cleanupRecipientStage = (*filepublish.Stage).Cleanup

var recipientPackageTool = toolDefinition{
	name: "export_recipient_package", title: "Export recipient package", write: true, destructive: true,
	description: "Save a recipient ZIP from completed numbered reviews using a CLI selection file on the MCP host. " +
		"Requires --allow-package-writes. Check the selection and destination with the operator. " +
		"PDF masks, text and numbers are fully checked; no weaker check is substituted. " +
		"Use stdio for packages that may exceed HTTP MCP's two-minute deadline. " +
		"A package can hold the process's full PDF check slot for 30 minutes. " +
		"If the reply is lost, inspect the destination before retrying; existing files are never replaced.",
	schemas: recipientPackageSchemas,
}

type recipientPackageInput struct {
	SelectionPath   string `json:"selection_path"`
	DestinationPath string `json:"destination_path"`
}

type recipientPackageOutput struct {
	privateCache

	State           string                             `json:"state"`
	DestinationPath string                             `json:"destination_path"`
	Size            int64                              `json:"size"`
	SHA256          string                             `json:"sha256"`
	ReceiptSHA256   string                             `json:"receipt_sha256"`
	Verification    string                             `json:"verification"`
	Receipt         processing.RecipientPackageReceipt `json:"receipt"`
	CleanupFailed   bool                               `json:"cleanup_failed"`
}

func exportRecipientPackage(ctx context.Context, lease *daemonLease, raw []byte, logger *slog.Logger) (output recipientPackageOutput, err error) {
	var input recipientPackageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return output, err
	}
	if input.SelectionPath == "" || len(input.SelectionPath) > maxPathBytes || !filepath.IsAbs(input.SelectionPath) || filepath.Clean(input.SelectionPath) != input.SelectionPath {
		return output, invalidToolArgumentsError()
	}
	if err := validateExportDestination(input.DestinationPath, false); err != nil {
		return output, recipientPackageLocalError(ctx, err)
	}
	if !reviewedBatesActive.CompareAndSwap(false, true) {
		return output, &reviewedBatesError{code: "redaction_busy"}
	}
	defer reviewedBatesActive.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	selection, err := daemonconn.ReadRecipientSelection(ctx, input.SelectionPath)
	if err != nil {
		return output, recipientPackageError(ctx, err)
	}
	c, err := lease.transferConnection(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return output, recipientPackageError(ctx, err)
		}
		return output, daemonAcquisitionError(err)
	}
	defer func() { _ = lease.close(c) }()
	checked, err := daemonconn.PreflightRecipientPackage(ctx, c, selection)
	if err != nil {
		return output, recipientPackageError(ctx, err)
	}
	stage, err := filepublish.CreateStage(filepath.Dir(input.DestinationPath), "docbank-recipient-")
	if err != nil {
		return output, recipientPackageLocalError(ctx, err)
	}
	defer func() {
		if cleanupErr := cleanupRecipientStage(stage); cleanupErr != nil {
			logger.Warn("MCP recipient stage cleanup failed", "error_code", recipientPackageLocalIOCode, "error", cleanupErr)
			output.CleanupFailed = true
			if output.State == "" {
				err = errors.Join(err, recipientPackageLocalError(ctx, cleanupErr))
			}
		}
	}()
	archive, err := daemonconn.BuildRecipientPackage(ctx, c, checked, stage.File)
	if err != nil {
		return output, recipientPackageError(ctx, err)
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return output, recipientPackageLocalError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return output, recipientPackageError(ctx, err)
	}
	published, publishErr := filepublish.Publish(stage.Path(), input.DestinationPath, false)
	if !published {
		return output, recipientPackageLocalError(ctx, publishErr)
	}
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
		logger.Warn("MCP recipient package saved with uncertain durability", "error_code", recipientPackageLocalIOCode, "error", publishErr)
	}
	return recipientPackageOutput{privateCache: newPrivateCache(), State: state,
		DestinationPath: input.DestinationPath, Size: archive.Size, SHA256: archive.SHA256,
		ReceiptSHA256: archive.ReceiptSHA256, Verification: "masks_text_and_numbers", Receipt: archive.Receipt}, nil
}

func recipientPackageError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil, errors.Is(err, pdfproduction.ErrVerifierVersion), errors.Is(err, processing.ErrRedactionReviewChanged):
		return reviewedBatesDownloadError(ctx, err)
	case errors.Is(err, processing.ErrRecipientPackageLimit):
		return &reviewedBatesError{code: recipientPackageLimitCode}
	case errors.Is(err, processing.ErrRecipientPackageInvalid):
		return invalidToolArgumentsError()
	case errors.Is(err, processing.ErrRecipientPackageIntegrity):
		return &reviewedBatesError{code: "redaction_integrity"}
	}
	if _, private := errors.AsType[*daemonconn.RecipientPackageInputError](err); private {
		return invalidToolArgumentsError()
	}
	if _, local := errors.AsType[*os.PathError](err); local {
		return recipientPackageLocalError(ctx, err)
	}
	return reviewedBatesDownloadError(ctx, err)
}

func recipientPackageLocalError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return reviewedBatesDownloadError(ctx, err)
	}
	if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
		return invalid
	}
	return &reviewedBatesError{code: recipientPackageLocalIOCode, cause: err}
}

func recipientPackageHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := exportRecipientPackage(ctx, lease, request.Params.Arguments, logger)
		if err != nil {
			logOperationError(logger, recipientPackageTool.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok && invalid.Code == jsonrpc.CodeInvalidParams {
				return nil, invalid
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
