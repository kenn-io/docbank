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
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const toolCatalogTTLMs = 60_000

func catalogInstructions(allowProcessing bool) string {
	if allowProcessing {
		return "Docbank exposes bounded reads plus guarded write tools; Bates reservation does not stamp or publish files, and processing still requires prior operator consent for the exact plan."
	}
	return "Docbank exposes a bounded read-only document surface."
}

type toolDefinition struct {
	name        string
	title       string
	description string
	schemas     func() (schema, schema)
	write       bool
	idempotent  bool
	destructive bool
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
	{name: "get_package_import", title: "Get package import", description: "Read durable progress for one load-file import operation.", schemas: getPackageImportSchemas},
	{name: "get_package_preflight", title: "Get package preflight", description: "Read one exact retained load-file package preflight.", schemas: getPackagePreflightSchemas},
	{name: "list_package_preflight_diagnostics", title: "List package preflight diagnostics", description: "Page through bounded diagnostics for one retained package preflight.", schemas: listPackagePreflightDiagnosticsSchemas},
	{name: "list_package_custodians", title: "List package custodians", description: "Page through active custodian claims for an exact package scope.", schemas: listPackageCustodiansSchemas},
	{name: "find_people", title: "Find people", description: "Find bounded canonical person candidates before resolving a custodian claim.", schemas: findPeopleSchemas},
	{name: "list_packages", title: "List packages", description: "Page through received and produced load-file packages.", schemas: listPackagesSchemas},
	{name: "get_package", title: "Get package", description: "Read one load-file package and its retained source authority.", schemas: getPackageSchemas},
	{name: "list_package_members", title: "List package members", description: "Page through one package's immutable document occurrences.", schemas: listPackageMembersSchemas},
	{name: "get_package_record", title: "Get package record", description: "Read one immutable sender row by its package-scoped record key.", schemas: getPackageRecordSchemas},
	{name: "lookup_bates_label", title: "Look up Bates label", description: "Find every bounded package-scoped match for an exact received or assigned label.", schemas: lookupBatesLabelSchemas},
	{name: "list_bates_namespaces", title: "List Bates namespaces", description: "Page through bounded Bates label namespaces.", schemas: listBatesNamespacesSchemas},
	{name: "preview_bates_stamp", title: "Preview Bates stamp", description: "Preview tentative Bates labels for one sealed snapshot without reserving or stamping anything.", schemas: previewBatesStampSchemas},
	{name: "get_bates_allocation", title: "Get Bates allocation", description: "Read one exact Bates allocation.", schemas: getBatesAllocationSchemas},
	{name: "list_bates_exports", title: "List Bates exports", description: "Page through bounded verified Bates export history.", schemas: listBatesExportsSchemas},
	{name: "get_bates_export", title: "Get Bates export", description: "Read one exact verified Bates export receipt.", schemas: getBatesExportSchemas},
	{name: "find_bates_exports", title: "Find Bates exports", description: "Return bounded candidates for one exact Bates label, custodian label, or canonical person.", schemas: findBatesExportsSchemas},
}

var processingToolDefinition = toolDefinition{
	name: "start_processing", title: "Start processing",
	description: "Queue processing only for an exact pre-reviewed plan with prior operator consent.",
	schemas:     startProcessingSchemas, write: true,
}

var packageImportToolDefinition = toolDefinition{
	name: "start_package_import", title: "Start package import",
	description: "Start or replay a reviewed load-file import using its preflight identity and operation UUID.",
	schemas:     startPackageImportSchemas, write: true, idempotent: true,
}

var preflightLoadFilePackageToolDefinition = toolDefinition{
	name: "preflight_load_file_package", title: "Preflight load-file package",
	description: "Retain a reviewed preflight for a local load-file directory or ZIP before import.",
	schemas:     preflightLoadFilePackageSchemas, write: true,
}

var resolvePackageCustodianToolDefinition = toolDefinition{
	name: "resolve_package_custodian", title: "Resolve package custodian",
	description: "Link one exact existing package custodian claim to one exact canonical person.",
	schemas:     resolvePackageCustodianSchemas, write: true,
}

var assignPackageCustodianToolDefinition = toolDefinition{
	name: "assign_package_custodian", title: "Assign package custodian",
	description: "Create or replace the operator-owned primary custodian for one exact package scope.",
	schemas:     assignPackageCustodianSchemas, write: true, destructive: true,
}

var ensureBatesNamespaceToolDefinition = toolDefinition{
	name: "ensure_bates_namespace", title: "Ensure Bates namespace",
	description: "Create or find the exact Bates label namespace.",
	schemas:     ensureBatesNamespaceSchemas, write: true, idempotent: true,
}

var reserveBatesRangeToolDefinition = toolDefinition{
	name: "reserve_bates_range", title: "Reserve Bates range",
	description: "Reserve one idempotent Bates range for a sealed snapshot without stamping files.",
	schemas:     reserveBatesRangeSchemas, write: true, idempotent: true,
}

var publishBatesExportToolDefinition = toolDefinition{
	name: "publish_bates_export", title: "Publish Bates export",
	description: "Publish one bounded verified Bates PDF from an exact reserved allocation and reviewed recipe.",
	schemas:     publishBatesExportSchemas, write: true, idempotent: true,
}

var exportBatesFileToolDefinition = toolDefinition{
	name: "export_bates_file", title: "Export Bates file",
	description: "Write one exact retained Bates PDF to a local path after independent verification.",
	schemas:     exportBatesFileSchemas, write: true, destructive: true,
}

var exportLoadFilePackageToolDefinition = toolDefinition{
	name: "export_load_file_package", title: "Export load-file package",
	description: "Write one exact verified load-file package to a local path.",
	schemas:     exportLoadFilePackageSchemas, write: true, destructive: true,
}

func toolCatalog(allowProcessing bool) []*sdkmcp.Tool {
	definitions := readToolDefinitions
	if allowProcessing {
		definitions = append(slices.Clone(definitions), processingToolDefinition, preflightLoadFilePackageToolDefinition,
			packageImportToolDefinition, resolvePackageCustodianToolDefinition, assignPackageCustodianToolDefinition,
			ensureBatesNamespaceToolDefinition, reserveBatesRangeToolDefinition, publishBatesExportToolDefinition,
			exportBatesFileToolDefinition, exportLoadFilePackageToolDefinition)
	}
	tools := make([]*sdkmcp.Tool, 0, len(definitions))
	for _, definition := range definitions {
		input, output := definition.schemas()
		openWorld := definition.write
		annotation := &sdkmcp.ToolAnnotations{
			Title: definition.title, ReadOnlyHint: !definition.write,
			IdempotentHint: !definition.write || definition.idempotent, DestructiveHint: &definition.destructive, OpenWorldHint: &openWorld,
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
	server *sdkmcp.Server, allowProcessing bool, lease *daemonLease, plans *processingPlanRegistry, logger *slog.Logger,
) {
	tools := toolCatalog(allowProcessing)
	server.AddReceivingMiddleware(validateToolInputs(tools))
	for _, tool := range tools {
		output := mustResolveSchema(tool.OutputSchema)
		var handler sdkmcp.ToolHandler
		switch tool.Name {
		case processingToolDefinition.name:
			handler = processingToolHandler(lease, plans, output, logger)
		case packageImportToolDefinition.name:
			handler = packageImportToolHandler(lease, output, logger)
		case preflightLoadFilePackageToolDefinition.name:
			handler = packagePreflightToolHandler(lease, output, logger)
		case resolvePackageCustodianToolDefinition.name, assignPackageCustodianToolDefinition.name:
			handler = packageCustodianWriteToolHandler(lease, tool.Name, output, logger)
		case ensureBatesNamespaceToolDefinition.name, reserveBatesRangeToolDefinition.name,
			publishBatesExportToolDefinition.name, exportBatesFileToolDefinition.name:
			handler = batesWriteToolHandler(lease, tool.Name, output, logger)
		case exportLoadFilePackageToolDefinition.name:
			handler = packageExportToolHandler(lease, output, logger)
		default:
			handler = readToolHandler(lease, plans, tool.Name, output, logger)
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
	case errors.Is(err, errBatesOutcomeUnknown):
		return "bates_outcome_unknown", 0
	case errors.Is(err, errDaemonUnavailable):
		return "daemon_unavailable", 0
	case errors.Is(err, store.ErrDocumentCursorExpired):
		return "cursor_expired", 0
	case errors.Is(err, store.ErrInvalidDocumentCursor):
		return "invalid_document_cursor", 0
	case errors.Is(err, store.ErrBatesReservationConflict):
		return "bates_reservation_conflict", 0
	case errors.Is(err, store.ErrBatesPageCountMismatch):
		return "bates_page_count_mismatch", 0
	case errors.Is(err, store.ErrBatesOverflow):
		return "bates_overflow", 0
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
	case "bates_reservation_conflict", "bates_page_count_mismatch", "bates_overflow":
		return facts.Code, 0
	default:
		return "", 0
	}
}

func domainErrorMessage(code string) string {
	switch code {
	case "not_found":
		return "The requested Docbank identity was not found."
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
	case "bates_reservation_conflict":
		return "The Bates reservation conflicts with existing namespace or idempotency authority."
	case "bates_page_count_mismatch":
		return "The sealed snapshot pages no longer match the Bates request."
	case "bates_overflow":
		return "The Bates range exceeds the namespace padding."
	case "bates_outcome_unknown":
		return "The Bates authority write outcome is unknown; reconcile the namespace or allocation before retrying."
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
