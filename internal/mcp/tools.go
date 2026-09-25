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

func catalogInstructions(allowProcessing, allowPackageWrites bool, writes ...bool) string {
	exportWrites := len(writes) > 0 && writes[0]
	reportWrites := len(writes) > 1 && writes[1]
	if !allowProcessing && !allowPackageWrites && !exportWrites && !reportWrites {
		return "Docbank exposes bounded read-only document, package, report, and export operations."
	}
	instructions := "Docbank exposes bounded document, package, report, and export reads."
	if allowProcessing {
		instructions += " start_processing requires a reviewed plan and prior operator consent."
	}
	if allowPackageWrites {
		instructions += " Package writes can preflight local sources, import packages, and assign or resolve custodians."
	}
	if exportWrites {
		instructions += " Export writes require exact selected document identities and explicit operation IDs."
	}
	if reportWrites {
		instructions += " Report writes create frozen selections and reviewed date revisions."
	}
	return instructions
}

type toolDefinition struct {
	name          string
	title         string
	description   string
	schemas       func() (schema, schema)
	write         bool
	idempotent    bool
	nonIdempotent bool
	destructive   bool
}

var readToolDefinitions = []toolDefinition{
	{name: "get_agent_capabilities", title: "Get agent capabilities", description: "Discover operations currently available to this credential.", schemas: getAgentCapabilitiesSchemas},
	{name: "get_vault_info", title: "Get vault info", description: "Summarize the selected vault without exposing its host path.", schemas: getVaultInfoSchemas},
	{name: "list_documents", title: "List documents", description: "Page through current, live documents with bounded stable ordering.", schemas: listDocumentsSchemas},
	{name: "list_tags", title: "List tags", description: "Page through visible tag definitions and assignment counts.", schemas: listTagsSchemas},
	{name: "search_documents", title: "Search documents", description: "Search an exact bounded current-version source fence and report coverage.", schemas: searchDocumentsSchemas},
	{name: "get_document", title: "Get document", description: "Read metadata for one exact current document identity.", schemas: getDocumentSchemas},
	{name: "list_document_versions", title: "List document versions", description: "Page through immutable content versions for one stable document node.", schemas: listDocumentVersionsSchemas},
	{name: "read_rendition_text", title: "Read rendition text", description: "Read a bounded Unicode window from an active sanitized Markdown rendition.", schemas: readRenditionTextSchemas},
	{name: "get_processing_plan", title: "Get processing plan", description: "Preview the exact provider disclosure and consent state for one document version.", schemas: getProcessingPlanSchemas},
	{name: "get_processing_status", title: "Get processing status", description: "Read the current state of one stable processing job.", schemas: getProcessingStatusSchemas},
	{name: "get_processing_coverage", title: "Get processing coverage", description: "Read rendition and embedding coverage for an exact source fence.", schemas: getProcessingCoverageSchemas},
	{name: "list_processing_profiles", title: "List processing profiles", description: "List locally executable processing profiles without provider credentials.", schemas: listProcessingProfilesSchemas},
	{name: "get_format_coverage", title: "Get format coverage", description: "Read the verified per-format capability inventory and optional exact lookup.", schemas: getFormatCoverageSchemas},
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
	{name: "preview_export_plan", title: "Preview export plan", description: "Inspect frozen role availability before starting a native document export.", schemas: previewExportPlanSchemas},
	{name: "get_export_plan", title: "Get export plan", description: "Read the immutable header of one native document export plan.", schemas: getExportPlanSchemas},
	{name: "list_export_output_problems", title: "List export output problems", description: "Page through unavailable output details for one frozen export plan.", schemas: listExportOutputProblemsSchemas},
	{name: "get_export_job", title: "Get export job", description: "Read bounded progress and the retained receipt for one native export job.", schemas: getExportJobSchemas},
	{name: "open_export_archive", title: "Open export archive", description: "Verify a completed native export and open a bounded private archive handle.", schemas: openExportArchiveSchemas, nonIdempotent: true},
	{name: "download_export_archive", title: "Download export archive", description: "Read a bounded chunk after checking the source is still visible.", schemas: downloadExportArchiveSchemas, nonIdempotent: true},
	{name: "open_report_artifact", title: "Open report artifact", description: "Open an owner-checked frozen CSV or bundle artifact and return a short-lived handle.", schemas: openReportArtifactSchemas, nonIdempotent: true},
	{name: "download_report_artifact", title: "Download report artifact", description: "Read a verified bounded chunk of a frozen report artifact; authorization is checked on every call.", schemas: downloadReportArtifactSchemas, nonIdempotent: true},
	{name: "get_report_summary", title: "Get report summary", description: "Read an owner-checked frozen report summary with current source visibility.", schemas: getReportSummarySchemas},
	{name: "list_report_history", title: "List report history", description: "Page through owner-checked retained report requests and summaries with current source visibility.", schemas: listReportHistorySchemas},
	{name: "get_report_dates", title: "Get report dates", description: "Page through bounded frozen date evidence for one report.", schemas: getReportDatesSchemas},
}

var exportWriteToolDefinitions = []toolDefinition{
	{name: "create_export_source", title: "Create export source", description: "Freeze up to 100 exact selected document versions for export.", schemas: createExportSourceSchemas, write: true, idempotent: true},
	{name: "create_export_plan", title: "Create export plan", description: "Bind a frozen source and selected output roles to an export plan.", schemas: createExportPlanSchemas, write: true, idempotent: true},
	{name: "start_export_job", title: "Start export job", description: "Start one durable export from an exact previewed plan fingerprint.", schemas: startExportJobSchemas, write: true, idempotent: true},
	{name: "cancel_export_job", title: "Cancel export job", description: "Cancel one queued or running native export job.", schemas: cancelExportJobSchemas, write: true, idempotent: true, destructive: true},
}

var reportWriteToolDefinitions = []toolDefinition{
	{name: "create_report", title: "Create report", description: "Freeze an exact native document selection and its search-term evidence.", schemas: createReportSchemas, write: true},
	{name: "revise_report", title: "Revise report", description: "Create a new frozen report using exact reviewed date choices.", schemas: reviseReportSchemas, write: true},
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

func toolCatalog(allowProcessing, allowPackageWrites bool, writes ...bool) []*sdkmcp.Tool {
	definitions := slices.Clone(readToolDefinitions)
	if allowProcessing {
		definitions = append(definitions, processingToolDefinition)
	}
	if allowPackageWrites {
		definitions = append(definitions, preflightLoadFilePackageToolDefinition, packageImportToolDefinition,
			resolvePackageCustodianToolDefinition, assignPackageCustodianToolDefinition)
	}
	if len(writes) > 0 && writes[0] {
		definitions = append(definitions, exportWriteToolDefinitions...)
	}
	if len(writes) > 1 && writes[1] {
		definitions = append(definitions, reportWriteToolDefinitions...)
	}
	tools := make([]*sdkmcp.Tool, 0, len(definitions))
	for _, definition := range definitions {
		input, output := definition.schemas()
		openWorld := definition.write
		annotation := &sdkmcp.ToolAnnotations{
			Title: definition.title, ReadOnlyHint: !definition.write,
			IdempotentHint:  !definition.nonIdempotent && (!definition.write || definition.idempotent),
			DestructiveHint: &definition.destructive, OpenWorldHint: &openWorld,
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
	server *sdkmcp.Server, allowProcessing, allowPackageWrites, allowExportWrites, allowReportWrites bool,
	lease *daemonLease, plans *processingPlanRegistry, archives *exportArchiveRegistry,
	policy operationPolicy, logger *slog.Logger,
) *reportHandleSigner {
	tools := toolCatalog(allowProcessing, allowPackageWrites, allowExportWrites, allowReportWrites)
	reportHandles := newReportHandleSigner()
	if !policy.local() {
		tools = slices.DeleteFunc(tools, func(tool *sdkmcp.Tool) bool {
			switch tool.Name {
			case "get_agent_capabilities", "get_vault_info", "list_documents", "list_tags", "search_documents", "get_document",
				"read_rendition_text", "get_processing_plan", "get_processing_status",
				"get_processing_coverage", "list_processing_profiles", "get_format_coverage", "start_processing":
				return false
			default:
				return true
			}
		})
	}
	server.AddReceivingMiddleware(validateToolInputs(tools))
	for _, tool := range tools {
		output := mustResolveSchema(tool.OutputSchema)
		var handler sdkmcp.ToolHandler
		switch tool.Name {
		case processingToolDefinition.name:
			handler = processingToolHandler(lease, plans, policy, output, logger)
		case packageImportToolDefinition.name:
			handler = packageImportToolHandler(lease, output, logger)
		case preflightLoadFilePackageToolDefinition.name:
			handler = packagePreflightToolHandler(lease, output, logger)
		case resolvePackageCustodianToolDefinition.name, assignPackageCustodianToolDefinition.name:
			handler = packageCustodianWriteToolHandler(lease, tool.Name, output, logger)
		case "preview_export_plan", "get_export_plan", "list_export_output_problems", "get_export_job", "create_export_source", "create_export_plan", "start_export_job", "cancel_export_job", "open_export_archive", "download_export_archive":
			handler = exportToolHandler(lease, archives, tool.Name, output, logger)
		case "open_report_artifact", "download_report_artifact":
			handler = reportToolHandler(lease, reportHandles, tool.Name, output, logger)
		case "get_report_summary", "list_report_history", "get_report_dates", "create_report", "revise_report":
			handler = reportControlToolHandler(lease, tool.Name, output, logger)
		default:
			handler = readToolHandler(lease, plans, policy, tool.Name, output, logger)
		}
		server.AddTool(tool, handler)
	}
	return reportHandles
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
	case errors.Is(err, store.ErrExportVisibilityChanged):
		return "visibility_changed", 0
	case errors.Is(err, errExportArchiveHandle):
		return "artifact_unavailable", 0
	case errors.Is(err, errExportArchiveCapacity):
		return "artifact_limit", 0
	case errors.Is(err, errReportHandleUnavailable):
		return "report_unavailable", 0
	case errors.Is(err, errReportSpoolCapacity):
		return "report_capacity", 0
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
	case "report_unavailable", "visibility_changed":
		return facts.Code, 0
	case "access_denied", "unauthorized", "forbidden":
		return "access_denied", 0
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
	case "visibility_changed":
		return "The source is no longer visible."
	case "access_denied":
		return "The current credential cannot access this Docbank operation."
	case "artifact_unavailable":
		return "The archive handle is unavailable; open the archive again."
	case "artifact_limit":
		return "The archive exceeds the MCP byte limit; use the CLI download."
	case "report_unavailable":
		return "The report artifact is unavailable to this owner."
	case "report_capacity":
		return "Report download capacity is exhausted; close a handle or retry later."
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
