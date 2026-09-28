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
	{name: "find_production_numbers", title: "Find production numbers", description: "Resolve an exact published production number or page through a bounded numeric range.", schemas: findProductionNumbersSchemas},
	{name: "find_production_number_candidates", title: "Find production number candidates", description: "Find verified published outputs for exact, prefix, or substring label text and report ambiguity.", schemas: findProductionNumberCandidatesSchemas},
	{name: "list_production_policies", title: "List production policies", description: "Page through immutable production policy versions with bounded summaries.", schemas: listProductionPoliciesSchemas},
	{name: "get_production_policy", title: "Get production policy", description: "Read one exact immutable production policy version.", schemas: getProductionPolicySchemas},
	{name: "get_production_approval", title: "Get production approval", description: "Read an approval grant and lifecycle events without private evidence or reasons.", schemas: getProductionApprovalSchemas},
	{name: "get_production_privilege_log", title: "Get production privilege log", description: "Page the public rows of one exact frozen privilege log.", schemas: getProductionPrivilegeLogSchemas},
	{name: "get_production_supplement", title: "Get production supplement", description: "Read one exact verified production continuation record.", schemas: getProductionSupplementSchemas},
	{name: "get_production_reproduction", title: "Get production reproduction", description: "Read one exact verified reproduction receipt for a published production.", schemas: getProductionReproductionSchemas},
	{name: "get_production_package", title: "Get production package", description: "Read one exact verified retained recipient package evidence receipt.", schemas: getProductionPackageSchemas},
	{name: "list_production_sets", title: "List production sets", description: "Page through bounded production set summaries.", schemas: listProductionSetsSchemas},
	{name: "get_production_set", title: "Get production set", description: "Read one exact production set.", schemas: getProductionSetSchemas},
	{name: "get_production_draft", title: "Get production draft", description: "Read one exact production revision and ETag.", schemas: getProductionDraftSchemas},
	{name: "list_production_members", title: "List production members", description: "Page through bounded member summaries for an exact revision.", schemas: listProductionMembersSchemas},
	{name: "list_production_decisions", title: "List production decisions", description: "Page through bounded decision summaries for an exact revision.", schemas: listProductionDecisionsSchemas},
	{name: "get_production_job", title: "Get production job", description: "Read one exact production job status.", schemas: getProductionJobSchemas},
	{name: "list_production_recipes", title: "List production recipes", description: "Read qualified rendering recipe identities and digests.", schemas: listProductionRecipesSchemas},
	{name: "resolve_production_selection", title: "Resolve production selection", description: "Read one bounded resolved mask page and its review binding for an exact draft ETag.", schemas: resolveProductionSelectionSchemas},
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

var createProductionPolicyToolDefinition = toolDefinition{
	name: "create_production_policy", title: "Create production policy",
	description: "Create or replay one immutable policy version with an explicit operation UUID.",
	schemas:     createProductionPolicySchemas, write: true, idempotent: true,
}

var validateProductionPrivilegeLogToolDefinition = toolDefinition{
	name: "validate_production_privilege_log", title: "Validate production privilege log",
	description: "Validate stored draft rows against exact withheld, policy, and player authority.",
	schemas:     validateProductionPrivilegeLogSchemas, write: true, idempotent: true,
}

var freezeProductionPrivilegeLogToolDefinition = toolDefinition{
	name: "freeze_production_privilege_log", title: "Freeze production privilege log",
	description: "Freeze an exact validated draft after the daemon rechecks stored rows and required approval.",
	schemas:     freezeProductionPrivilegeLogSchemas, write: true, idempotent: true,
}

var replaceProductionPrivilegeRowsToolDefinition = toolDefinition{
	name: "replace_production_privilege_rows", title: "Replace production privilege rows",
	description: "Replace private rows of an exact draft generation from a local JSON file.",
	schemas:     replaceProductionPrivilegeRowsSchemas, write: true, idempotent: true,
}

var createProductionPrivilegeDraftToolDefinition = toolDefinition{
	name: "create_production_privilege_draft", title: "Create production privilege draft",
	description: "Create an exact privilege-log draft from sealed authority and a local private row file.",
	schemas:     createProductionPrivilegeDraftSchemas, write: true, idempotent: true,
}

var exportProductionPrivilegeLogToolDefinition = toolDefinition{
	name: "export_production_privilege_log", title: "Export production privilege log",
	description: "Write verified public bytes of one frozen privilege log to a local file.",
	schemas:     exportProductionPrivilegeLogSchemas, write: true, destructive: true,
}

var downloadProductionPackageToolDefinition = toolDefinition{
	name: "download_production_package", title: "Download production package",
	description: "Save an exact retained production archive to a local file after complete byte verification.",
	schemas:     downloadProductionPackageSchemas, write: true, destructive: true,
}

var createProductionSupplementToolDefinition = toolDefinition{
	name: "create_production_supplement", title: "Create production supplement",
	description: "Reserve the exact next number range for a prepared child of a published production.",
	schemas:     createProductionSupplementSchemas, write: true, idempotent: true,
}

var createProductionReproductionToolDefinition = toolDefinition{
	name: "create_production_reproduction", title: "Create production reproduction",
	description: "Build and retain a fresh verified package from the complete published artifact selection.",
	schemas:     createProductionReproductionSchemas, write: true, idempotent: true,
}

var createProductionPackageToolDefinition = toolDefinition{
	name: "create_production_package", title: "Create production package",
	description: "Build and retain a recipient archive from one published production with a stable operation ID.",
	schemas:     createProductionPackageSchemas, write: true, idempotent: true,
}

var createProductionPlayersSnapshotToolDefinition = toolDefinition{
	name: "create_production_players_snapshot", title: "Create production players snapshot",
	description: "Record a versioned player and alias snapshot from a private local JSON file.",
	schemas:     createProductionPlayersSnapshotSchemas, write: true, idempotent: true,
}

var createProductionWithheldSelectionToolDefinition = toolDefinition{
	name: "create_production_withheld_selection", title: "Create production withheld selection",
	description: "Record an explicit version-pinned withheld selection from a private local JSON file.",
	schemas:     createProductionWithheldSelectionSchemas, write: true, idempotent: true,
}

var createProductionSetToolDefinition = toolDefinition{
	name: "create_production_set", title: "Create production set",
	description: "Create or replay one production set with an explicit operation UUID.",
	schemas:     createProductionSetSchemas, write: true, idempotent: true,
}

var forkProductionDraftToolDefinition = toolDefinition{
	name: "fork_production_draft", title: "Fork production draft",
	description: "Create or replay a new editable revision from an exact source revision.",
	schemas:     forkProductionDraftSchemas, write: true, idempotent: true,
}

var editProductionInstructionsToolDefinition = toolDefinition{
	name: "edit_production_instructions", title: "Edit production instructions",
	description: "Replace revision instructions under an exact ETag and operation UUID.",
	schemas:     editProductionInstructionsSchemas, write: true, idempotent: true,
}

var sealProductionMembershipToolDefinition = toolDefinition{
	name: "seal_production_membership", title: "Seal production membership",
	description: "Seal the exact member count and digest under an ETag and operation UUID.",
	schemas:     sealProductionMembershipSchemas, write: true, idempotent: true,
}

var reviewProductionMemberToolDefinition = toolDefinition{
	name: "review_production_member", title: "Review production member",
	description: "Declare a member fully reviewed with the daemon-derived binding.",
	schemas:     reviewProductionMemberSchemas, write: true, idempotent: true,
}

var selectProductionGateAuthorityToolDefinition = toolDefinition{
	name: "select_production_gate_authority", title: "Select production gate authority",
	description: "Pin an existing verified approval and frozen privilege log to a sealed revision.",
	schemas:     selectProductionGateAuthoritySchemas, write: true, idempotent: true,
}

var appendProductionMembersToolDefinition = toolDefinition{
	name: "append_production_members", title: "Append production members",
	description: "Append a bounded JSON member batch to an exact draft ETag with an operation UUID.",
	schemas:     appendProductionMembersSchemas, write: true, idempotent: true,
}

var applyProductionChangesToolDefinition = toolDefinition{
	name: "apply_production_changes", title: "Apply production changes",
	description: "Apply a bounded JSON change batch to an exact draft ETag with an operation UUID.",
	schemas:     applyProductionChangesSchemas, write: true, idempotent: true,
}

var finalizeProductionDraftToolDefinition = toolDefinition{
	name: "finalize_production_draft", title: "Finalize production draft",
	description: "Finalize one fully reviewed revision under its exact ETag and operation UUID.",
	schemas:     finalizeProductionDraftSchemas, write: true, idempotent: true,
}

var admitProductionJobToolDefinition = toolDefinition{
	name: "admit_production_job", title: "Admit production job",
	description: "Admit or replay one job from finalized authority with explicit job and operation UUIDs.",
	schemas:     admitProductionJobSchemas, write: true, idempotent: true,
}

var cancelProductionJobToolDefinition = toolDefinition{
	name: "cancel_production_job", title: "Cancel production job",
	description: "Cancel one exact production job with its ETag and operation UUID.",
	schemas:     cancelProductionJobSchemas, write: true, idempotent: true, destructive: true,
}

var publishProductionPackageToolDefinition = toolDefinition{
	name: "publish_production_package", title: "Publish production package",
	description: "Publish or replay one verified recipient package from a successful job.",
	schemas:     publishProductionPackageSchemas, write: true, idempotent: true,
}

func toolCatalog(allowProcessing bool) []*sdkmcp.Tool {
	definitions := readToolDefinitions
	if allowProcessing {
		definitions = append(slices.Clone(definitions), processingToolDefinition, preflightLoadFilePackageToolDefinition,
			packageImportToolDefinition, resolvePackageCustodianToolDefinition, assignPackageCustodianToolDefinition,
			ensureBatesNamespaceToolDefinition, reserveBatesRangeToolDefinition, publishBatesExportToolDefinition,
			exportBatesFileToolDefinition, exportLoadFilePackageToolDefinition, createProductionPolicyToolDefinition,
			validateProductionPrivilegeLogToolDefinition, replaceProductionPrivilegeRowsToolDefinition,
			createProductionPrivilegeDraftToolDefinition, freezeProductionPrivilegeLogToolDefinition,
			exportProductionPrivilegeLogToolDefinition, downloadProductionPackageToolDefinition,
			createProductionSupplementToolDefinition, createProductionReproductionToolDefinition,
			createProductionPackageToolDefinition, createProductionPlayersSnapshotToolDefinition,
			createProductionWithheldSelectionToolDefinition,
			createProductionSetToolDefinition, forkProductionDraftToolDefinition,
			editProductionInstructionsToolDefinition, sealProductionMembershipToolDefinition,
			reviewProductionMemberToolDefinition, appendProductionMembersToolDefinition,
			selectProductionGateAuthorityToolDefinition,
			applyProductionChangesToolDefinition, finalizeProductionDraftToolDefinition,
			admitProductionJobToolDefinition, cancelProductionJobToolDefinition,
			publishProductionPackageToolDefinition)
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
		case createProductionPolicyToolDefinition.name:
			handler = createProductionPolicyToolHandler(lease, output, logger)
		case validateProductionPrivilegeLogToolDefinition.name:
			handler = validateProductionPrivilegeLogToolHandler(lease, output, logger)
		case freezeProductionPrivilegeLogToolDefinition.name:
			handler = freezeProductionPrivilegeLogToolHandler(lease, output, logger)
		case replaceProductionPrivilegeRowsToolDefinition.name:
			handler = replaceProductionPrivilegeRowsToolHandler(lease, output, logger)
		case createProductionPrivilegeDraftToolDefinition.name:
			handler = createProductionPrivilegeDraftToolHandler(lease, output, logger)
		case exportProductionPrivilegeLogToolDefinition.name:
			handler = exportProductionPrivilegeLogToolHandler(lease, output, logger)
		case downloadProductionPackageToolDefinition.name:
			handler = downloadProductionPackageToolHandler(lease, output, logger)
		case createProductionSupplementToolDefinition.name:
			handler = createProductionSupplementToolHandler(lease, output, logger)
		case createProductionReproductionToolDefinition.name:
			handler = createProductionReproductionToolHandler(lease, output, logger)
		case createProductionPackageToolDefinition.name:
			handler = createProductionPackageToolHandler(lease, output, logger)
		case createProductionPlayersSnapshotToolDefinition.name:
			handler = createProductionPlayersSnapshotToolHandler(lease, output, logger)
		case createProductionWithheldSelectionToolDefinition.name:
			handler = createProductionWithheldSelectionToolHandler(lease, output, logger)
		case selectProductionGateAuthorityToolDefinition.name:
			handler = selectProductionGateAuthorityToolHandler(lease, output, logger)
		case createProductionSetToolDefinition.name, forkProductionDraftToolDefinition.name:
			handler = productionDraftWriteToolHandler(lease, tool.Name, output, logger)
		case editProductionInstructionsToolDefinition.name, sealProductionMembershipToolDefinition.name,
			reviewProductionMemberToolDefinition.name:
			handler = productionReviewWriteToolHandler(lease, tool.Name, output, logger)
		case appendProductionMembersToolDefinition.name, applyProductionChangesToolDefinition.name:
			handler = productionChangesWriteToolHandler(lease, tool.Name, output, logger)
		case finalizeProductionDraftToolDefinition.name, admitProductionJobToolDefinition.name,
			cancelProductionJobToolDefinition.name:
			handler = productionJobWriteToolHandler(lease, tool.Name, output, logger)
		case publishProductionPackageToolDefinition.name:
			handler = productionPackagePublishToolHandler(lease, output, logger)
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
	case errors.Is(err, errProductionOutcomeUnknown):
		return "production_outcome_unknown", 0
	case errors.Is(err, errBatesOutcomeUnknown):
		return "bates_outcome_unknown", 0
	case errors.Is(err, errProductionPolicyOutcomeUnknown):
		return "production_policy_outcome_unknown", 0
	case errors.Is(err, errProductionPrivilegeOutcomeUnknown):
		return "production_privilege_outcome_unknown", 0
	case errors.Is(err, errProductionSupplementOutcomeUnknown):
		return "production_supplement_outcome_unknown", 0
	case errors.Is(err, errProductionReproductionOutcomeUnknown):
		return "production_reproduction_outcome_unknown", 0
	case errors.Is(err, errProductionPackageOutcomeUnknown):
		return "production_package_outcome_unknown", 0
	case errors.Is(err, errProductionPlayersOutcomeUnknown):
		return "production_players_outcome_unknown", 0
	case errors.Is(err, errProductionWithheldOutcomeUnknown):
		return "production_withheld_outcome_unknown", 0
	case errors.Is(err, errProductionGateSelectionOutcomeUnknown):
		return "production_gate_selection_outcome_unknown", 0
	case errors.Is(err, errProductionPrivilegeExportDestinationExists):
		return "destination_exists", 0
	case errors.Is(err, errProductionPackageDestinationExists):
		return "destination_exists", 0
	case errors.Is(err, errProductionPackageDownloadFailed):
		return "package_download_failed", 0
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
	case "production_policy_conflict", "invalid_production_policy", "production_privilege_conflict", "invalid_production_privilege",
		"production_supplement_conflict", "production_reproduction_conflict",
		"invalid_production_reproduction", "production_reproduction_too_large",
		"production_package_conflict", "invalid_production_package", "production_package_too_large",
		"production_players_conflict", "invalid_production_players",
		"production_withheld_conflict", "invalid_production_withheld_selection",
		"approval_required", "approval_stale":
		return facts.Code, 0
	case "production_operation_conflict", "production_revision_conflict", "production_job_conflict",
		"production_numbering_conflict", "invalid_production", "source_stale", "decision_conflict",
		"selection_expansion_required", "mapping_incomplete", "invalid_mode", "render_limit",
		"changed_payload", "policy_unsatisfied", "privilege_log_required", "privilege_log_stale",
		"retention_required", "artifact_missing", "artifact_mismatch", "invalid_contract", "limit",
		"production_package_canceled", "production_package_timeout", "production_unavailable",
		"production_download_failed", "production_download_canceled", "production_download_timeout":
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
	case "production_outcome_unknown":
		return "The production write outcome is unknown; retry only with the same operation ID."
	case "production_gate_selection_outcome_unknown":
		return "The gate selection outcome is unknown; read the revision before retrying the same selection."
	case "production_operation_conflict":
		return "The operation ID names different production input."
	case "production_revision_conflict":
		return "The production revision changed. Read its current ETag before editing."
	case "production_job_conflict":
		return "The production job cannot be changed in its current state."
	case "production_numbering_conflict":
		return "The production numbering authority changed. Review the namespace before retrying."
	case "source_stale", "changed_payload", "privilege_log_stale", "artifact_missing", "artifact_mismatch":
		return "Production inputs or required evidence changed. Review current authority."
	case "policy_unsatisfied", "privilege_log_required", "retention_required":
		return "The selected production policy requires more evidence before this operation."
	case "decision_conflict", "selection_expansion_required", "mapping_incomplete", "invalid_mode":
		return "The production selection needs review before this operation."
	case "render_limit", "limit":
		return "The production input exceeds a configured limit."
	case "invalid_contract", "invalid_production":
		return "The production input is invalid."
	case "production_package_canceled", "production_package_timeout":
		return "Package publication stopped before a result was confirmed. Retry with the same operation ID."
	case "production_unavailable":
		return "The production package publisher is unavailable."
	case "production_download_failed":
		return "The package download failed verification. No destination file was published."
	case "production_download_canceled", "production_download_timeout":
		return "The production package download stopped before a file was published."
	case "production_policy_conflict":
		return "The operation ID or policy version conflicts with existing immutable policy authority."
	case "invalid_production_policy":
		return "The production policy input is invalid."
	case "production_policy_outcome_unknown":
		return "The policy write outcome is unknown; read the exact version or retry with the same operation ID."
	case "production_privilege_conflict":
		return "The privilege-log draft changed or the operation ID names different input."
	case "invalid_production_privilege":
		return "The privilege-log validation input is invalid."
	case "approval_required":
		return "This policy requires current human approval before the privilege log can be frozen."
	case "approval_stale":
		return "The stored human approval no longer matches this privilege-log revision."
	case "production_privilege_outcome_unknown":
		return "The privilege-log mutation outcome is unknown; retry only with the exact same operation ID and input."
	case "production_supplement_conflict":
		return "The supplement parent, prepared child or operation ID conflicts with current production authority."
	case "production_supplement_outcome_unknown":
		return "The supplement write outcome is unknown; read the operation ID or retry only with identical input."
	case "production_reproduction_conflict":
		return "The reproduction selection, policy, original job or operation ID conflicts with published authority."
	case "invalid_production_reproduction":
		return "The reproduction request or package limits are invalid."
	case "production_reproduction_too_large":
		return "The selected reproduction package exceeds the supported retained archive size."
	case "production_reproduction_outcome_unknown":
		return "The reproduction outcome is unknown; read the exact job and operation ID before retrying identical input."
	case "production_package_conflict":
		return "The recipient package conflicts with the published production or retained evidence."
	case "invalid_production_package":
		return "The package operation, profile or volume limits are invalid."
	case "production_package_too_large":
		return "The selected recipient package exceeds the supported retained archive size."
	case "production_package_outcome_unknown":
		return "The package outcome is unknown; read the exact job and operation ID before retrying identical input."
	case "production_players_conflict":
		return "The player snapshot operation conflicts with stored authority."
	case "invalid_production_players":
		return "The player snapshot is invalid."
	case "production_players_outcome_unknown":
		return "The player snapshot outcome is unknown; retry only with the same operation ID and identical input."
	case "production_withheld_conflict":
		return "The withheld selection conflicts with sealed production authority."
	case "invalid_production_withheld_selection":
		return "The withheld selection is invalid."
	case "production_withheld_outcome_unknown":
		return "The withheld selection outcome is unknown; retry only with the same operation ID and identical input."
	case "destination_exists":
		return "The export destination already exists; choose another path or explicitly allow overwrite."
	case "package_download_failed":
		return "The retained production archive could not be verified or saved."
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
