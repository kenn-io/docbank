package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const toolCatalogTTLMs = 60_000

func catalogInstructions(allowProcessing bool) string {
	if allowProcessing {
		return "Docbank exposes bounded reads plus guarded start_processing; processing still requires prior operator consent for the exact plan."
	}
	return "Docbank exposes a bounded read-only document surface."
}

type toolDefinition struct {
	name        string
	title       string
	description string
	schemas     func() (schema, schema)
	write       bool
}

var readToolDefinitions = []toolDefinition{
	{name: "get_vault_info", title: "Get vault info", description: "Summarize the selected vault without exposing its host path.", schemas: getVaultInfoSchemas},
	{name: "list_documents", title: "List documents", description: "Page through current, live documents with bounded stable ordering.", schemas: listDocumentsSchemas},
	{name: "search_documents", title: "Search documents", description: "Search an exact bounded current-version source fence and report coverage.", schemas: searchDocumentsSchemas},
	{name: "get_document", title: "Get document", description: "Read metadata for one exact current document identity.", schemas: getDocumentSchemas},
	{name: "list_document_versions", title: "List document versions", description: "Page through immutable content versions for one stable document node.", schemas: listDocumentVersionsSchemas},
	{name: "read_rendition_text", title: "Read rendition text", description: "Read a bounded Unicode window from an active sanitized Markdown rendition.", schemas: readRenditionTextSchemas},
	{name: "get_processing_plan", title: "Get processing plan", description: "Preview the exact provider disclosure and consent state for one document version.", schemas: getProcessingPlanSchemas},
	{name: "get_processing_status", title: "Get processing status", description: "Read the current state of one stable processing job.", schemas: getProcessingStatusSchemas},
	{name: "get_processing_coverage", title: "Get processing coverage", description: "Read rendition and embedding coverage for an exact source fence.", schemas: getProcessingCoverageSchemas},
}

var processingToolDefinition = toolDefinition{
	name: "start_processing", title: "Start processing",
	description: "Queue processing only for an exact pre-reviewed plan with prior operator consent.",
	schemas:     startProcessingSchemas, write: true,
}

func toolCatalog(allowProcessing bool) []*sdkmcp.Tool {
	definitions := readToolDefinitions
	if allowProcessing {
		definitions = append(slices.Clone(definitions), processingToolDefinition)
	}
	nonDestructive := false
	tools := make([]*sdkmcp.Tool, 0, len(definitions))
	for _, definition := range definitions {
		input, output := definition.schemas()
		openWorld := definition.write
		annotation := &sdkmcp.ToolAnnotations{
			Title: definition.title, ReadOnlyHint: !definition.write,
			IdempotentHint: !definition.write, DestructiveHint: &nonDestructive, OpenWorldHint: &openWorld,
		}
		tools = append(tools, &sdkmcp.Tool{
			Name: definition.name, Title: definition.title, Description: definition.description,
			Annotations: annotation, InputSchema: input, OutputSchema: output,
			Meta: sdkmcp.Meta{"io.docbank/bounds": map[string]any{"maxResponseBytes": maxToolResponseBytes}},
		})
	}
	return tools
}

func registerToolCatalog(
	server *sdkmcp.Server, allowProcessing bool, lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, logger *slog.Logger,
) {
	tools := toolCatalog(allowProcessing)
	server.AddReceivingMiddleware(validateToolInputs(tools))
	for _, tool := range tools {
		output := mustResolveSchema(tool.OutputSchema)
		handler := processingToolHandler(lease, plans, policy, output, logger)
		if tool.Name != processingToolDefinition.name {
			handler = readToolHandler(lease, plans, policy, tool.Name, output, logger)
		}
		server.AddTool(tool, handler)
	}
}

func validateToolInputs(tools []*sdkmcp.Tool) func(sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	validators := make(map[string]*jsonschema.Resolved, len(tools))
	for _, tool := range tools {
		validators[tool.Name] = mustResolveSchema(tool.InputSchema)
	}
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, request)
			}
			call, ok := request.(*sdkmcp.CallToolRequest)
			if !ok || call.Params == nil {
				return nil, invalidToolArgumentsError()
			}
			validator, exists := validators[call.Params.Name]
			if !exists {
				return next(ctx, method, request)
			}
			arguments, err := decodeToolArguments(call.Params.Arguments)
			if err != nil || validator.Validate(&arguments) != nil || !validToolSemantics(call.Params.Name, arguments) {
				return nil, invalidToolArgumentsError()
			}
			return next(ctx, method, request)
		}
	}
}

func decodeToolArguments(raw jsontext.Value) (map[string]any, error) {
	arguments := map[string]any{}
	if len(raw) == 0 {
		return arguments, nil
	}
	if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
		return nil, errors.New("invalid tool arguments")
	}
	return arguments, nil
}

func validToolSemantics(name string, arguments map[string]any) bool {
	switch name {
	case "list_documents":
		return stringBytesWithin(arguments, "path_prefix", maxPathBytes) &&
			stringBytesWithin(arguments, "cursor", maxCursorBytes)
	case "search_documents":
		filters, ok := arguments["filters"].(map[string]any)
		if !ok {
			return arguments["filters"] == nil
		}
		return validOptionalRFC3339Nano(filters, "modified_since") &&
			validOptionalRFC3339Nano(filters, "modified_before")
	default:
		return true
	}
}

func stringBytesWithin(arguments map[string]any, field string, maximum int) bool {
	value, ok := arguments[field]
	if !ok {
		return true
	}
	text, ok := value.(string)
	return ok && len(text) <= maximum
}

func validOptionalRFC3339Nano(arguments map[string]any, field string) bool {
	value, ok := arguments[field]
	if !ok {
		return true
	}
	text, ok := value.(string)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, text)
	return err == nil
}

func mustResolveSchema(raw any) *jsonschema.Resolved {
	data, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	var value jsonschema.Schema
	if err := json.Unmarshal(data, &value); err != nil {
		panic(err)
	}
	resolved, err := value.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		panic(err)
	}
	return resolved
}

func invalidToolArgumentsError() *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tool arguments"}
}

func normalizeToolCatalog(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil || method != "tools/list" {
			return result, err
		}
		catalog, ok := result.(*sdkmcp.ListToolsResult)
		if !ok {
			return nil, sanitizedRPCError(errors.New("tools/list returned an invalid result"))
		}
		catalog.TTLMs = toolCatalogTTLMs
		catalog.CacheScope = "public"
		return catalog, nil
	}
}

type toolErrorOutput struct {
	Code               string `json:"code"`
	Message            string `json:"message"`
	ObservedScopeCount int    `json:"observed_scope_count,omitzero"`
}

func domainToolError(err error) (*sdkmcp.CallToolResult, bool) {
	code, observed := stableDomainError(err)
	if code == "" {
		return nil, false
	}
	output := toolErrorOutput{Code: code, Message: domainErrorMessage(code), ObservedScopeCount: observed}
	encoded, marshalErr := json.Marshal(output)
	if marshalErr != nil || len(encoded) > maxToolErrorBytes {
		return nil, false
	}
	return &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(encoded)}},
		StructuredContent: output,
		IsError:           true,
	}, true
}

func stableDomainError(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	var scope *daemonconn.SourceFenceScopeTooLargeError
	switch {
	case errors.Is(err, api.ErrOperationNotFound):
		return "not_found", 0
	case errors.Is(err, api.ErrOperationDenied), errors.Is(err, api.ErrOperationGrantExpired), errors.Is(err, api.ErrOperationGrantRevoked):
		return "operation_denied", 0
	case errors.Is(err, api.ErrOperationScopeTooLarge):
		return "scope_too_large", 0
	case errors.As(err, &scope):
		return "scope_too_large", scope.ObservedScopeCount
	case errors.Is(err, store.ErrNotFound):
		return "not_found", 0
	case errors.Is(err, store.ErrProcessingSourceFenceStaleVersion):
		return "stale_version", 0
	case errors.Is(err, daemonconn.ErrProcessingPlanChanged):
		return "plan_changed", 0
	case errors.Is(err, daemonconn.ErrProcessingConsent):
		return "consent_required", 0
	case errors.Is(err, errProcessingOutcomeUnknown):
		return "processing_outcome_unknown", 0
	case errors.Is(err, errDaemonUnavailable):
		return "daemon_unavailable", 0
	case errors.Is(err, store.ErrDocumentCursorExpired):
		return "cursor_expired", 0
	case errors.Is(err, store.ErrInvalidDocumentCursor):
		return "invalid_document_cursor", 0
	}
	facts, ok := daemonProblemFacts(err)
	if !ok {
		facts, ok = daemonconn.ExtractProblemFacts(err)
	}
	if !ok {
		return "", 0
	}
	switch facts.Code {
	case "not_found":
		return "not_found", 0
	case "stale_version":
		return "stale_version", 0
	case "processing_plan_changed":
		return "plan_changed", 0
	case "processing_consent_required", "processing_consent_expired", "processing_consent_revoked":
		return "consent_required", 0
	case "scope_too_large":
		return "scope_too_large", facts.ObservedScopeCount
	case "cursor_expired":
		return "cursor_expired", 0
	case "invalid_document_cursor":
		return "invalid_document_cursor", 0
	case "invalid_rendition_window":
		return "invalid_rendition_window", 0
	case "invalid_rendition_encoding":
		return "invalid_rendition_encoding", 0
	default:
		return "", 0
	}
}

func domainErrorMessage(code string) string {
	switch code {
	case "not_found":
		return "The requested Docbank identity was not found."
	case "operation_denied":
		return "The authenticated principal is not permitted to perform this operation."
	case "stale_version":
		return "The requested content version is no longer current and live."
	case "plan_changed":
		return "The reviewed processing plan is missing or no longer matches the current disclosure."
	case "consent_required":
		return "Processing requires prior operator consent for this exact plan."
	case "processing_outcome_unknown":
		return "The initial processing outcome is unknown and has no MCP job ID; do not retry blindly."
	case "daemon_unavailable":
		return "The local Docbank daemon is unavailable; check the daemon and retry."
	case "scope_too_large":
		return "The exact source scope exceeds the supported limit; narrow the source scope."
	case "cursor_expired":
		return "The document cursor expired; restart the listing from the first page."
	case "invalid_document_cursor":
		return "The document cursor is invalid for this listing."
	case "invalid_rendition_window":
		return "The requested rendition text window is outside the supported range."
	case "invalid_rendition_encoding":
		return "The active rendition is not valid UTF-8 text."
	default:
		return "The Docbank operation could not be completed."
	}
}

func sanitizedRPCError(_ error) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal Docbank MCP error"}
}

func logOperationError(logger *slog.Logger, operation string, err error) {
	code, _ := stableDomainError(err)
	if code == "" {
		code = "internal_error"
		if boundary, ok := errors.AsType[*daemonBoundaryError](err); ok {
			code = boundary.diagnostic
		}
		switch {
		case errors.Is(err, errToolResultTooLarge):
			code = "result_too_large"
		case errors.Is(err, context.Canceled):
			code = "request_canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline_exceeded"
		case errors.Is(err, errDaemonCredentialReuse):
			code = "daemon_credential_reuse"
		}
	}
	logger.Error("MCP operation failed", "operation", operation, "error_code", code)
}
