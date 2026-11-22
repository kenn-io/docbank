package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

const preparationFileLimit = 16 << 20

const (
	preparationChanged   = "redaction_selection_changed"
	preparationIntegrity = "redaction_preparation_integrity"
	preparationLimit     = "redaction_preparation_limit"
	preparationLocalIO   = "redaction_preparation_local_io"
)

var errPreparationLocalIO = errors.New("saving private PDF preparation")

type preparationError string

func (err preparationError) Error() string { return string(err) }

var preparationMessages = map[string]string{
	preparationChanged:                     "Refresh the document and select its exact retained version again.",
	preparationIntegrity:                   "The PDF preparation failed its source or evidence checks.",
	preparationLimit:                       "The PDF or its preparation exceeds the size or work limits.",
	preparationLocalIO:                     "The preparation could not be saved. Check the destination and available disk space.",
	"redaction_preparation_unavailable":    "Preparation needs a daemon with the Linux amd64 page runtime configured.",
	"redaction_preparation_busy":           "Another native PDF operation is active in the daemon. Retry when it finishes.",
	"redaction_preparation_unsupported":    "The selected PDF does not support native preparation.",
	"redaction_preparation_memory":         "The daemon exceeds the renderer memory ceiling. Retry when it is quieter.",
	"redaction_preparation_canceled":       "PDF preparation was canceled.",
	"redaction_preparation_timeout":        "PDF preparation exceeded its request deadline.",
	"redaction_preparation_worker_timeout": "The daemon PDF worker exceeded its inspection deadline.",
	"redaction_preparation_failed":         "The daemon could not prepare the PDF. Check its log.",
}

var prepareNativePDFTool = toolDefinition{
	name: "prepare_native_pdf", title: "Prepare a PDF for review", write: true,
	description: "Save the private preparation JSON for one exact retained PDF version on the MCP host. " +
		"Get the vault from get_vault_info and version identity from list_document_versions. " +
		"With --allow-package-writes, choose a new absolute path outside the Docbank data directory. " +
		"The file contains unredacted text. Use it with docbank redaction resolve and explicit masks.",
	schemas: prepareNativePDFSchemas,
}

type prepareNativePDFInput struct {
	api.OriginalDownloadSelection

	ContentVersionID string `json:"content_version_id"`
	DestinationPath  string `json:"destination_path"`
}

type prepareNativePDFPage struct {
	Number        int            `json:"number"`
	Span          redaction.Span `json:"span"`
	OrderedGaps   int            `json:"ordered_gaps"`
	UnorderedGaps int            `json:"unordered_gaps"`
}

type prepareNativePDFOutput struct {
	privateCache

	VaultID          string                 `json:"vault_id"`
	NodeID           int64                  `json:"node_id"`
	ContentVersionID string                 `json:"content_version_id"`
	BlobHash         string                 `json:"blob_hash"`
	SourceSize       int64                  `json:"source_size"`
	Revision         int64                  `json:"revision"`
	DestinationPath  string                 `json:"destination_path"`
	Size             int                    `json:"size"`
	SHA256           string                 `json:"sha256"`
	EvidenceSHA256   string                 `json:"evidence_sha256"`
	MapSHA256        string                 `json:"map_sha256"`
	Pages            []prepareNativePDFPage `json:"pages"`
	State            string                 `json:"state"`
	CleanupFailed    bool                   `json:"cleanup_failed"`
}

func prepareNativePDF(ctx context.Context, lease *daemonLease, raw []byte, logger *slog.Logger) (prepareNativePDFOutput, error) {
	var input prepareNativePDFInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return prepareNativePDFOutput{}, err
	}
	if len(input.DestinationPath) > maxPathBytes {
		return prepareNativePDFOutput{}, invalidToolArgumentsError()
	}
	if err := validateExportDestination(input.DestinationPath, false); err != nil {
		return prepareNativePDFOutput{}, preparationDestinationError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err := lease.transferConnection(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return prepareNativePDFOutput{}, preparationRequestError(ctx, err)
		}
		return prepareNativePDFOutput{}, daemonAcquisitionError(err)
	}
	defer func() { _ = lease.close(c) }()
	selection, err := input.pageBinding(ctx, c)
	if err != nil {
		return prepareNativePDFOutput{}, preparationRequestError(ctx, err)
	}
	prepared, err := c.PrepareNativePDF(ctx, processing.RedactionPreparationRequest{Selection: selection})
	if err != nil {
		return prepareNativePDFOutput{}, preparationRequestError(ctx, err)
	}
	if err := production.ValidateNativePreparationDerivation(prepared.NativePreparation); err != nil {
		return prepareNativePDFOutput{}, preparationRequestError(ctx, errors.Join(daemonconn.ErrIntegrity, err))
	}
	data, err := json.Marshal(prepared)
	if err != nil {
		return prepareNativePDFOutput{}, preparationError(preparationIntegrity)
	}
	if len(data) > preparationFileLimit {
		return prepareNativePDFOutput{}, preparationError(preparationLimit)
	}
	return saveNativePreparation(ctx, logger, input, prepared, data)
}

func (in prepareNativePDFInput) pageBinding(ctx context.Context, c *daemonconn.Connection) (store.PageBinding, error) {
	vault, err := c.API().VaultIdentity(ctx)
	if err != nil {
		return store.PageBinding{}, err
	}
	if !strings.EqualFold(in.VaultID, vault.VaultID) {
		return store.PageBinding{}, preparationError(preparationChanged)
	}
	node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: in.NodeID}})
	if err != nil {
		return store.PageBinding{}, err
	}
	version, err := c.API().GetContentVersion(ctx, &apiclient.GetContentVersionRequestOptions{
		PathParams: &apiclient.GetContentVersionPath{VersionID: in.ContentVersionID},
	})
	if err != nil {
		return store.PageBinding{}, err
	}
	if node.Path == "" || !in.Matches(vault.VaultID, in.ContentVersionID, *node, *version) {
		return store.PageBinding{}, preparationError(preparationChanged)
	}
	return store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{
		VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size,
	}}, nil
}

func saveNativePreparation(ctx context.Context, logger *slog.Logger, input prepareNativePDFInput,
	prepared processing.RedactionPreparation, data []byte,
) (output prepareNativePDFOutput, _ error) {
	if err := ctx.Err(); err != nil {
		return output, preparationRequestError(ctx, err)
	}
	stage, err := filepublish.CreateStage(filepath.Dir(input.DestinationPath), "docbank-preparation-")
	if err != nil {
		return output, preparationDestinationError(err)
	}
	defer func() {
		if cleanupErr := stage.Cleanup(); cleanupErr != nil {
			logger.Warn("MCP PDF preparation stage cleanup failed", "error", cleanupErr)
			output.CleanupFailed = true
		}
	}()
	if _, err := stage.File.Write(data); err != nil {
		return output, preparationDestinationError(err)
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return output, preparationDestinationError(err)
	}
	if err := ctx.Err(); err != nil {
		return output, preparationRequestError(ctx, err)
	}
	published, err := filepublish.Publish(stage.Path(), input.DestinationPath, false)
	if !published {
		return output, preparationDestinationError(err)
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
		logger.Warn("MCP PDF preparation saved with uncertain durability", "error", err)
	}
	pages := make([]prepareNativePDFPage, len(prepared.Map.Pages))
	for i, page := range prepared.Map.Pages {
		pages[i] = prepareNativePDFPage{Number: page.Number, Span: page.Span}
	}
	for _, gap := range prepared.Map.Gaps {
		page := &pages[gap.Box.Page-1]
		if gap.Unordered {
			page.UnorderedGaps++
		} else {
			page.OrderedGaps++
		}
	}
	return prepareNativePDFOutput{privateCache: newPrivateCache(), State: state, VaultID: input.VaultID,
		NodeID: input.NodeID, ContentVersionID: input.ContentVersionID, BlobHash: input.BlobHash,
		SourceSize: input.Size, Revision: prepared.Selection.Revision, DestinationPath: input.DestinationPath,
		Size: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), EvidenceSHA256: prepared.EvidenceSHA256,
		MapSHA256: prepared.Map.SHA256, Pages: pages}, nil
}

func preparationRequestError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return preparationError("redaction_preparation_timeout")
	case ctx.Err() != nil:
		return preparationError("redaction_preparation_canceled")
	case errors.Is(err, pdfproduction.ErrVerifierVersion):
		return preparationError("redaction_verifier_version")
	case errors.Is(err, daemonconn.ErrIntegrity), errors.Is(err, processing.ErrRedactionPreparationInvalid):
		return preparationError(preparationIntegrity)
	case daemonconn.IsResponseDecodeError(err):
		return preparationError(preparationIntegrity)
	}
	if refusal, ok := errors.AsType[preparationError](err); ok {
		return refusal
	}
	code, _ := daemonconn.ProblemCode(err)
	if _, known := preparationMessages[code]; known || code == "not_found" {
		return preparationError(code)
	}
	return sanitizedDaemonError(errDaemonRequestFailed, err)
}

func preparationDestinationError(err error) error {
	if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
		return invalid
	}
	return errors.Join(errPreparationLocalIO, err)
}

func prepareNativePDFHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := prepareNativePDF(ctx, lease, request.Params.Arguments, logger)
		if err != nil {
			logOperationError(logger, prepareNativePDFTool.name, err)
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

func prepareNativePDFSchemas() (schema, schema) {
	const node = "node_id"
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	input := rootObjectSchema(schema{
		schemaVaultIDField: uuidSchema(), node: integerSchema(1, 0), versionField: uuidSchema(),
		"blob_hash": sha256Schema(), schemaSizeField: integerSchema(1, document.MaxPageSourceBytes), destinationPathField: path,
	}, schemaVaultIDField, node, versionField, "blob_hash", schemaSizeField, destinationPathField)
	span := objectSchema(schema{"start": integerSchema(0, preparationFileLimit), "end": integerSchema(0, preparationFileLimit)}, "start", "end")
	page := objectSchema(schema{
		"number": integerSchema(1, document.MaxRenderPages), "span": span,
		"ordered_gaps": integerSchema(0, 0), "unordered_gaps": integerSchema(0, 0),
	}, "number", "span", "ordered_gaps", "unordered_gaps")
	return input, rootObjectSchema(withPrivateCache(schema{
		schemaVaultIDField: uuidSchema(), node: integerSchema(1, 0), versionField: uuidSchema(),
		"blob_hash": sha256Schema(), "source_size": integerSchema(1, document.MaxPageSourceBytes), schemaRevisionField: integerSchema(1, 0),
		destinationPathField: path, schemaSizeField: integerSchema(1, preparationFileLimit), "sha256": sha256Schema(),
		"evidence_sha256": sha256Schema(), "map_sha256": sha256Schema(), "pages": arraySchema(page, document.MaxRenderPages),
		schemaStateField: enumSchema("published", "published_durability_unknown"), "cleanup_failed": booleanSchema(),
	}), cacheRequired(schemaVaultIDField, node, versionField, "blob_hash", "source_size", schemaRevisionField, destinationPathField,
		schemaSizeField, "sha256", "evidence_sha256", "map_sha256", "pages", schemaStateField, "cleanup_failed")...)
}
