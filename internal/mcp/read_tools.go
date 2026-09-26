package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

var errToolResultTooLarge = errors.New("MCP tool result exceeds the response limit")

type privateCache struct {
	TTLMs      int    `json:"ttlMs"`
	CacheScope string `json:"cacheScope"`
}

func newPrivateCache() privateCache { return privateCache{CacheScope: "private"} }

func readToolHandler(
	lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executeReadToolWithPolicy(ctx, lease, plans, policy, name, validator, request.Params.Arguments)
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

func executeReadTool(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, name string, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	return executeReadToolWithPolicy(ctx, lease, plans, newOperationPolicy(nil, api.Principal{}), name, validator, raw)
}

func executeReadToolWithPolicy(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, name string, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	operation := api.OperationRead
	switch name {
	case "search_documents", "get_processing_coverage":
		operation = api.OperationAnalyze
	case "get_processing_plan", "get_processing_status":
		operation = api.OperationProcessing
	}
	scope, err := policy.authorize(ctx, operation, nil, false, false)
	if err != nil {
		return nil, err
	}
	var output any
	var links []*sdkmcp.ResourceLink
	switch name {
	case "get_agent_capabilities":
		output, err = getAgentCapabilities(ctx, lease, raw)
	case "get_vault_info":
		output, err = getVaultInfoScoped(ctx, lease, policy, scope, raw)
	case "list_documents":
		output, links, err = listDocumentsScoped(ctx, lease, policy, scope, raw)
	case "list_tags":
		output, err = listTags(ctx, lease, raw)
	case "search_documents":
		output, links, err = searchDocumentsScoped(ctx, lease, policy, raw)
	case "get_document":
		output, links, err = getDocumentScoped(ctx, lease, policy, raw)
	case "list_document_versions":
		output, err = listDocumentVersionsScoped(ctx, lease, policy, raw)
	case "read_rendition_text":
		output, err = readRenditionTextScoped(ctx, lease, policy, raw)
	case "get_processing_plan":
		output, err = getProcessingPlanScoped(ctx, lease, policy, raw)
	case "get_processing_status":
		output, err = getProcessingStatusScoped(ctx, lease, plans, policy, raw)
	case "get_processing_coverage":
		output, err = getProcessingCoverageScoped(ctx, lease, policy, raw)
	case "list_processing_profiles":
		output, err = listProcessingProfiles(ctx, lease, raw)
	case "get_format_coverage":
		output, err = getFormatCoverage(ctx, lease, raw)
	case "get_package_import":
		output, err = getPackageImport(ctx, lease, raw)
	case "get_package_preflight":
		output, err = getPackagePreflight(ctx, lease, raw)
	case "list_package_preflight_diagnostics":
		output, err = listPackagePreflightDiagnostics(ctx, lease, raw)
	case "list_package_custodians":
		output, err = listPackageCustodians(ctx, lease, raw)
	case "find_people":
		output, err = findPeople(ctx, lease, raw)
	case "list_packages":
		output, err = listPackages(ctx, lease, raw)
	case "get_package":
		output, err = getPackage(ctx, lease, raw)
	case "list_package_members":
		output, err = listPackageMembers(ctx, lease, raw)
	case "get_package_record":
		output, err = getPackageRecord(ctx, lease, raw)
	case "lookup_bates_label":
		output, err = lookupBatesLabel(ctx, lease, raw)
	default:
		return nil, errors.New("unknown Docbank read tool")
	}
	if err != nil {
		return nil, err
	}
	result, err := boundedToolSuccess(validator, output, links)
	if err == nil && name == "get_processing_plan" {
		plan, ok := output.(processingPlanOutput)
		if !ok {
			return nil, errors.New("processing plan result has an invalid internal type")
		}
		plans.remember(plan.ProcessingPlan)
	}
	return result, err
}

type processingProfilesOutput struct {
	privateCache

	Profiles []api.ProcessingProfileSummary `json:"profiles"`
}

type listTagsInput struct {
	Limit  *int `json:"limit"`
	Offset int  `json:"offset"`
}

type listTagsOutput struct {
	api.TagPage
	privateCache
}

func listTags(ctx context.Context, lease *daemonLease, raw []byte) (listTagsOutput, error) {
	var input listTagsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listTagsOutput{}, err
	}
	limit := 100
	if input.Limit != nil {
		limit = *input.Limit
	}
	if limit < 1 || limit > 250 || input.Offset < 0 || input.Offset > 1_000_000 {
		return listTagsOutput{}, invalidToolArgumentsError()
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.TagPage, error) {
		return c.Tags(ctx, limit, input.Offset)
	})
	if err != nil {
		return listTagsOutput{}, err
	}
	return listTagsOutput{TagPage: page, privateCache: newPrivateCache()}, nil
}

func listProcessingProfiles(ctx context.Context, lease *daemonLease, raw []byte) (processingProfilesOutput, error) {
	var input struct{}
	if err := decodeReadArguments(raw, &input); err != nil {
		return processingProfilesOutput{}, err
	}
	profiles, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*apiclient.ListDocumentProcessingProfilesResponse, error) {
		return c.API().ListDocumentProcessingProfiles(ctx)
	})
	if err != nil {
		return processingProfilesOutput{}, err
	}
	if len(*profiles) > 128 {
		return processingProfilesOutput{}, errToolResultTooLarge
	}
	result := processingProfilesOutput{Profiles: make([]api.ProcessingProfileSummary, 0, len(*profiles)), privateCache: newPrivateCache()}
	for _, profile := range *profiles {
		profile.EmbeddingBindings = append([]string{}, profile.EmbeddingBindings...)
		profile.QueryEmbeddingBindings = append([]string{}, profile.QueryEmbeddingBindings...)
		result.Profiles = append(result.Profiles, profile)
	}
	return result, nil
}

type listPackagesInput struct {
	Direction string `json:"direction"`
	PageSize  int    `json:"page_size"`
	Cursor    string `json:"cursor"`
}

type listPackagesOutput struct {
	api.PackagePage
	privateCache
}

func listPackages(ctx context.Context, lease *daemonLease, raw []byte) (listPackagesOutput, error) {
	var input listPackagesInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listPackagesOutput{}, err
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackagePage, error) {
		query := &apiclient.ListPackagesQuery{After: optionalString(input.Cursor), Limit: optionalInt64(input.PageSize)}
		if input.Direction != "" {
			direction := apiclient.ListPackagesQueryDirection(input.Direction)
			query.Direction = &direction
		}
		return c.API().ListPackages(ctx, &apiclient.ListPackagesRequestOptions{Query: query})
	})
	if err != nil {
		return listPackagesOutput{}, err
	}
	return listPackagesOutput{PackagePage: *page, privateCache: newPrivateCache()}, nil
}

type packageIDInput struct {
	PackageID string `json:"package_id"`
}

type getPackageOutput struct {
	api.PackageDetail
	privateCache
}

func getPackage(ctx context.Context, lease *daemonLease, raw []byte) (getPackageOutput, error) {
	var input packageIDInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return getPackageOutput{}, err
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageDetail, error) {
		return c.API().GetPackage(ctx, &apiclient.GetPackageRequestOptions{PathParams: &apiclient.GetPackagePath{PackageID: input.PackageID}})
	})
	if err != nil {
		return getPackageOutput{}, err
	}
	return getPackageOutput{PackageDetail: *result, privateCache: newPrivateCache()}, nil
}

type listPackageMembersInput struct {
	PackageID string `json:"package_id"`
	After     int    `json:"after_ordinal"`
	PageSize  int    `json:"page_size"`
}

type listPackageMembersOutput struct {
	api.PackageMemberPage
	privateCache
}

func listPackageMembers(ctx context.Context, lease *daemonLease, raw []byte) (listPackageMembersOutput, error) {
	var input listPackageMembersInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listPackageMembersOutput{}, err
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageMemberPage, error) {
		return c.API().ListPackageMembers(ctx, &apiclient.ListPackageMembersRequestOptions{
			PathParams: &apiclient.ListPackageMembersPath{PackageID: input.PackageID},
			Query:      &apiclient.ListPackageMembersQuery{AfterOrdinal: optionalInt64(input.After), Limit: optionalInt64(input.PageSize)},
		})
	})
	if err != nil {
		return listPackageMembersOutput{}, err
	}
	return listPackageMembersOutput{PackageMemberPage: *page, privateCache: newPrivateCache()}, nil
}

type getPackageRecordInput struct {
	PackageID string `json:"package_id"`
	RowID     string `json:"row_id"`
}

type getPackageRecordOutput struct {
	api.PackageRecord
	privateCache
}

func getPackageRecord(ctx context.Context, lease *daemonLease, raw []byte) (getPackageRecordOutput, error) {
	var input getPackageRecordInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return getPackageRecordOutput{}, err
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageRecord, error) {
		return c.API().GetPackageRecord(ctx, &apiclient.GetPackageRecordRequestOptions{PathParams: &apiclient.GetPackageRecordPath{
			PackageID: input.PackageID, RowID: input.RowID,
		}})
	})
	if err != nil {
		return getPackageRecordOutput{}, err
	}
	return getPackageRecordOutput{PackageRecord: *result, privateCache: newPrivateCache()}, nil
}

type lookupBatesLabelInput struct {
	Label      string `json:"label"`
	PackageID  string `json:"package_id"`
	LabelSet   string `json:"label_set"`
	Provenance string `json:"provenance"`
	Cursor     string `json:"cursor"`
	PageSize   int    `json:"page_size"`
}

type lookupBatesLabelOutput struct {
	api.PackageLabelCandidatePage
	privateCache
}

func lookupBatesLabel(ctx context.Context, lease *daemonLease, raw []byte) (lookupBatesLabelOutput, error) {
	var input lookupBatesLabelInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return lookupBatesLabelOutput{}, err
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageLabelCandidatePage, error) {
		query := &apiclient.ListPackageLabelCandidatesQuery{Label: input.Label, PackageID: optionalString(input.PackageID),
			LabelSet: optionalString(input.LabelSet), Cursor: optionalString(input.Cursor), Limit: optionalInt64(input.PageSize)}
		if input.Provenance != "" {
			provenance := apiclient.ListPackageLabelCandidatesQueryProvenance(input.Provenance)
			query.Provenance = &provenance
		}
		return c.API().ListPackageLabelCandidates(ctx, &apiclient.ListPackageLabelCandidatesRequestOptions{Query: query})
	})
	if err != nil {
		return lookupBatesLabelOutput{}, err
	}
	return lookupBatesLabelOutput{PackageLabelCandidatePage: *page, privateCache: newPrivateCache()}, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalInt64(value int) *int64 {
	if value == 0 {
		return nil
	}
	converted := int64(value)
	return &converted
}

func decodeReadArguments(raw []byte, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, target, json.RejectUnknownMembers(true)); err != nil {
		return invalidToolArgumentsError()
	}
	return nil
}

type vaultInfoOutput struct {
	privateCache

	VaultID             string `json:"vault_id"`
	LiveFiles           int64  `json:"live_files"`
	LiveDirectories     int64  `json:"live_directories"`
	TrashedNodes        int64  `json:"trashed_nodes"`
	ContentVersions     int64  `json:"content_versions"`
	LogicalVersionBytes int64  `json:"logical_version_bytes"`
	TrackedBlobs        int64  `json:"tracked_blobs"`
	TrackedBlobBytes    int64  `json:"tracked_blob_bytes"`
}

func getVaultInfo(ctx context.Context, lease *daemonLease, raw []byte) (vaultInfoOutput, error) {
	var input struct{}
	if err := decodeReadArguments(raw, &input); err != nil {
		return vaultInfoOutput{}, err
	}
	info, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.VaultInfo, error) {
		return c.API().VaultInfo(ctx)
	})
	if err != nil {
		return vaultInfoOutput{}, err
	}
	return vaultInfoOutput{VaultID: info.VaultID, LiveFiles: info.LiveFiles,
		LiveDirectories: info.LiveDirectories, TrashedNodes: info.TrashedNodes,
		ContentVersions: info.ContentVersions, LogicalVersionBytes: info.LogicalVersionBytes,
		TrackedBlobs: info.TrackedBlobs, TrackedBlobBytes: info.TrackedBlobBytes,
		privateCache: newPrivateCache()}, nil
}

func getVaultInfoScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, scope api.OperationAuthorization, raw []byte) (vaultInfoOutput, error) {
	if policy.local() {
		return getVaultInfo(ctx, lease, raw)
	}
	if err := decodeReadArguments(raw, &struct{}{}); err != nil {
		return vaultInfoOutput{}, err
	}
	type scopedInfo struct {
		vaultID string
		bytes   int64
		blobs   map[string]int64
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (scopedInfo, error) {
		info, callErr := c.API().ReadCapabilities(ctx)
		if callErr != nil {
			return scopedInfo{}, callErr
		}
		out := scopedInfo{vaultID: info.VaultUID, blobs: make(map[string]int64)}
		for _, sourceID := range scope.SourceIDs {
			version, versionErr := c.API().GetContentVersion(ctx, &apiclient.GetContentVersionRequestOptions{PathParams: &apiclient.GetContentVersionPath{VersionID: sourceID}})
			if versionErr != nil {
				continue
			}
			out.bytes += version.Size
			out.blobs[version.BlobHash] = version.Size
		}
		return out, nil
	})
	if err != nil {
		return vaultInfoOutput{}, err
	}
	var blobBytes int64
	for _, size := range result.blobs {
		blobBytes += size
	}
	count := int64(len(scope.SourceIDs))
	return vaultInfoOutput{VaultID: result.vaultID, LiveFiles: count, ContentVersions: count,
		LogicalVersionBytes: result.bytes, TrackedBlobs: int64(len(result.blobs)), TrackedBlobBytes: blobBytes,
		privateCache: newPrivateCache()}, nil
}

type listDocumentsInput struct {
	PathPrefix string `json:"path_prefix"`
	Sort       string `json:"sort"`
	Direction  string `json:"direction"`
	PageSize   int    `json:"page_size"`
	Cursor     string `json:"cursor"`
}

type listDocumentsOutput struct {
	api.DocumentPage
	privateCache
}

func listDocuments(
	ctx context.Context, lease *daemonLease, raw []byte,
) (listDocumentsOutput, []*sdkmcp.ResourceLink, error) {
	var input listDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listDocumentsOutput{}, nil, err
	}
	type response struct {
		page api.DocumentPage
		info api.VaultInfo
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (response, error) {
		page, callErr := c.ListDocuments(ctx, api.DocumentQuery{PathPrefix: input.PathPrefix,
			Sort: input.Sort, Direction: input.Direction, PageSize: input.PageSize, Cursor: input.Cursor})
		if callErr != nil {
			return response{}, callErr
		}
		info, callErr := c.API().VaultInfo(ctx)

		if callErr != nil {
			return response{}, callErr
		}
		return response{page: page, info: *info}, nil
	})
	if err != nil {
		return listDocumentsOutput{}, nil, err
	}
	return listDocumentsOutput{DocumentPage: result.page, privateCache: newPrivateCache()},
		documentResourceLinks(result.info.VaultID, result.page.Items), nil
}

func listDocumentsScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, scope api.OperationAuthorization, raw []byte) (listDocumentsOutput, []*sdkmcp.ResourceLink, error) {
	if policy.local() {
		return listDocuments(ctx, lease, raw)
	}
	var input listDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listDocumentsOutput{}, nil, err
	}
	query := api.DocumentQuery{PathPrefix: input.PathPrefix, Sort: input.Sort,
		Direction: input.Direction, PageSize: input.PageSize, Cursor: input.Cursor}
	var page api.DocumentPage
	if len(scope.SourceIDs) == 0 {
		normalized, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{
			PathPrefix: query.PathPrefix, Sort: store.DocumentCatalogSort(query.Sort),
			Direction: store.DocumentCatalogDirection(query.Direction), PageSize: query.PageSize,
		})
		if err != nil || query.Cursor != "" {
			return listDocumentsOutput{}, nil, invalidToolArgumentsError()
		}
		page = api.DocumentPage{PathPrefix: normalized.PathPrefix, Sort: string(normalized.Sort),
			Direction: string(normalized.Direction), PageSize: normalized.PageSize,
			Items: []api.DocumentSummary{}}
	} else {
		var err error
		page, err = daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.DocumentPage, error) {
			return c.ListScopedDocuments(ctx, api.ScopedDocumentQuery{
				DocumentQuery: query, ContentVersionIDs: scope.SourceIDs})
		})
		if err != nil {
			return listDocumentsOutput{}, nil, err
		}
	}
	output := listDocumentsOutput{DocumentPage: page, privateCache: newPrivateCache()}
	var links []*sdkmcp.ResourceLink
	if len(page.Items) > 0 {
		info, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.Capabilities, error) {
			return c.API().ReadCapabilities(ctx)
		})
		if err != nil {
			return listDocumentsOutput{}, nil, err
		}
		links = documentResourceLinks(info.VaultUID, page.Items)
	}
	decision, err := policy.authorize(ctx, api.OperationRead, scope.SourceIDs, true, false)
	if err != nil {
		return listDocumentsOutput{}, nil, err
	}
	if !slices.Equal(decision.SourceIDs, scope.SourceIDs) {
		return listDocumentsOutput{}, nil, api.ErrOperationGrantRevoked
	}
	return output, links, nil
}

type searchDocumentsInput struct {
	Query             string                          `json:"query"`
	Mode              string                          `json:"mode"`
	Limit             int                             `json:"limit"`
	Profile           string                          `json:"profile"`
	BindingID         string                          `json:"binding_id"`
	Explain           bool                            `json:"explain"`
	ContentVersionIDs []string                        `json:"content_version_ids"`
	Filters           *api.DocumentSourceFenceFilters `json:"filters"`
}

type searchResultOutput struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Rank             int      `json:"rank"`
	Score            float64  `json:"score"`
	Path             string   `json:"path"`
	Excerpt          string   `json:"excerpt,omitempty"`
	EvidenceIDs      []string `json:"evidence_ids"`
}

type searchCoverageOutput struct {
	BindingRequired   bool   `json:"binding_required"`
	ScopedDocuments   int    `json:"scoped_documents"`
	CompleteDocuments int    `json:"complete_documents"`
	State             string `json:"state"`
}

type sourceFenceOutput struct {
	VaultID           string   `json:"vault_id"`
	ContentVersionIDs []string `json:"content_version_ids"`
}

type searchDocumentsOutput struct {
	privateCache

	VaultID            string               `json:"vault_id"`
	Fence              sourceFenceOutput    `json:"fence"`
	FenceFingerprint   string               `json:"fence_fingerprint"`
	ObservedScopeCount int                  `json:"observed_scope_count"`
	RequestedMode      string               `json:"requested_mode"`
	ActualMode         string               `json:"actual_mode"`
	Coverage           searchCoverageOutput `json:"coverage"`
	SkippedReasons     []string             `json:"skipped_reasons"`
	Results            []searchResultOutput `json:"results"`
	Truncated          bool                 `json:"truncated"`
}

func searchDocuments(
	ctx context.Context, lease *daemonLease, raw []byte, policy *operationPolicy,
) (searchDocumentsOutput, []*sdkmcp.ResourceLink, error) {
	var input searchDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return searchDocumentsOutput{}, nil, err
	}
	type response struct {
		resolution       api.DocumentSourceFenceResolution
		report           api.DocumentSearchReport
		documents        []api.DocumentSummary
		authorizationErr error
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (response, error) {
		resolution, callErr := c.ResolveDocumentSourceFence(ctx, api.DocumentSourceFenceResolveRequest{
			ContentVersionIDs: input.ContentVersionIDs, Filters: input.Filters})
		if callErr != nil {
			return response{}, callErr
		}
		if policy != nil && !policy.local() {
			decision, authErr := policy.authorize(ctx, api.OperationAnalyze,
				resolution.Fence.ContentVersionIDs, false, false)
			if authErr != nil {
				return response{authorizationErr: authErr}, nil //nolint:nilerr // Preserve the authorization error outside daemon error sanitization.
			}
			resolution.Fence.ContentVersionIDs = slices.Clone(decision.SourceIDs)
			resolution.ObservedScopeCount = len(decision.SourceIDs)
			resolution.FenceFingerprint, authErr = processing.SourceFenceFingerprint(processing.SourceFence{
				VaultUID: resolution.Fence.VaultUID, ContentVersionIDs: decision.SourceIDs,
			})
			if authErr != nil {
				return response{}, authErr
			}
		}
		if len(resolution.Fence.ContentVersionIDs) == 0 {
			callErr = c.ValidateDocumentSearch(ctx, api.DocumentSearchValidationRequest{
				Query: input.Query, Mode: input.Mode, Limit: input.Limit, Profile: input.Profile,
				BindingID: input.BindingID, Explain: input.Explain,
			})
			if callErr != nil {
				return response{}, callErr
			}
			return response{resolution: resolution, report: api.DocumentSearchReport{
				RequestedMode: effectiveSearchMode(input.Mode), ActualMode: effectiveEmptySearchMode(input.Mode),
				Coverage:     api.DocumentSearchCoverage{State: "complete"},
				Degradations: []string{}, Results: []api.DocumentSearchResult{}, Trace: []api.DocumentSearchTrace{},
			}}, nil
		}
		report, callErr := c.SearchDocuments(ctx, api.DocumentSearchRequest{Query: input.Query,
			Mode: input.Mode, Limit: input.Limit, Profile: input.Profile, BindingID: input.BindingID,
			Explain: input.Explain, Fence: api.DocumentSourceFence{VaultUID: resolution.Fence.VaultUID,
				ContentVersionIDs: slices.Clone(resolution.Fence.ContentVersionIDs)}})
		if callErr != nil {
			return response{}, callErr
		}
		if len(report.Results) == 0 {
			return response{resolution: resolution, report: report, documents: []api.DocumentSummary{}}, nil
		}
		identities := make([]api.DocumentIdentity, len(report.Results))
		for index, item := range report.Results {
			identities[index] = api.DocumentIdentity{NodeID: item.NodeID,
				ContentVersionID: item.ContentVersionID, Path: item.Path}
		}
		documents, callErr := c.ResolveDocumentSummaries(ctx,
			api.DocumentSummaryResolveRequest{Identities: identities})
		if callErr != nil {
			return response{}, callErr
		}
		return response{resolution: resolution, report: report, documents: documents}, nil
	})
	if err != nil {
		return searchDocumentsOutput{}, nil, err
	}
	if result.authorizationErr != nil {
		return searchDocumentsOutput{}, nil, result.authorizationErr
	}
	output := searchDocumentsOutput{VaultID: result.resolution.Fence.VaultUID,
		Fence: sourceFenceOutput{VaultID: result.resolution.Fence.VaultUID,
			ContentVersionIDs: slices.Clone(result.resolution.Fence.ContentVersionIDs)},
		FenceFingerprint:   result.resolution.FenceFingerprint,
		ObservedScopeCount: result.resolution.ObservedScopeCount,
		RequestedMode:      result.report.RequestedMode, ActualMode: result.report.ActualMode,
		Coverage: searchCoverageOutput{BindingRequired: result.report.Coverage.BindingRequired,
			ScopedDocuments:   result.report.Coverage.ScopedDocuments,
			CompleteDocuments: result.report.Coverage.CompleteDocuments, State: result.report.Coverage.State},
		SkippedReasons: slices.Clone(result.report.Degradations),
		Results:        make([]searchResultOutput, len(result.report.Results)), Truncated: result.report.Truncated,
		privateCache: newPrivateCache()}
	for index, item := range result.report.Results {
		evidence := make([]string, len(item.Evidence))
		for evidenceIndex, identity := range item.Evidence {
			encoded, marshalErr := json.Marshal(identity, json.Deterministic(true))
			if marshalErr != nil || len(encoded) > 1024 {
				return searchDocumentsOutput{}, nil, errors.New("search evidence identity is invalid")
			}
			evidence[evidenceIndex] = string(encoded)
		}
		output.Results[index] = searchResultOutput{NodeID: item.NodeID,
			ContentVersionID: item.ContentVersionID, Rank: item.Rank, Score: item.Score,
			Path: item.Path, Excerpt: item.Excerpt, EvidenceIDs: evidence}
	}
	return output, documentResourceLinks(result.resolution.Fence.VaultUID, result.documents), nil
}

func searchDocumentsScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (searchDocumentsOutput, []*sdkmcp.ResourceLink, error) {
	output, links, err := searchDocuments(ctx, lease, raw, &policy)
	if err != nil || policy.local() {
		return output, links, err
	}
	decision, err := policy.authorize(ctx, api.OperationAnalyze, output.Fence.ContentVersionIDs, true, false)
	if err != nil {
		return searchDocumentsOutput{}, nil, err
	}
	if !slices.Equal(decision.SourceIDs, output.Fence.ContentVersionIDs) {
		return searchDocumentsOutput{}, nil, api.ErrOperationGrantRevoked
	}
	allowed := make(map[string]bool, len(decision.SourceIDs))
	for _, id := range decision.SourceIDs {
		allowed[id] = true
	}
	return output, filterResourceLinks(links, allowed), nil
}

func effectiveSearchMode(mode string) string {
	if mode == "" {
		return "auto"
	}
	return mode
}

func effectiveEmptySearchMode(mode string) string {
	if mode == "semantic" || mode == "hybrid" {
		return mode
	}
	return "lexical"
}

type getDocumentInput struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
}

type documentOutput struct {
	privateCache

	NodeID           int64                           `json:"node_id"`
	ContentVersionID string                          `json:"content_version_id"`
	Path             string                          `json:"path"`
	Name             string                          `json:"name"`
	MediaType        string                          `json:"media_type"`
	Size             int64                           `json:"size"`
	ModifiedAt       string                          `json:"modified_at"`
	ActiveRenditions []api.DocumentRenditionIdentity `json:"active_renditions"`
}

func getDocument(
	ctx context.Context, lease *daemonLease, raw []byte,
) (documentOutput, []*sdkmcp.ResourceLink, error) {
	var input getDocumentInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return documentOutput{}, nil, err
	}
	type response struct {
		document api.DocumentSummary
		vaultID  string
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (response, error) {
		node, callErr := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: input.NodeID}})

		if callErr != nil {
			return response{}, callErr
		}
		if node.Kind != "file" || node.TrashedAt != "" || node.Path == "" ||
			node.CurrentVersionID != input.ContentVersionID {
			return response{}, store.ErrProcessingSourceFenceStaleVersion
		}
		page, callErr := c.ListDocuments(ctx, api.DocumentQuery{PathPrefix: node.Path, PageSize: 1})
		if callErr != nil {
			return response{}, callErr
		}
		if len(page.Items) != 1 || page.Items[0].NodeID != node.ID ||
			page.Items[0].ContentVersionID != input.ContentVersionID || page.Items[0].Path != node.Path {
			return response{}, store.ErrProcessingSourceFenceStaleVersion
		}
		info, callErr := c.API().VaultInfo(ctx)
		if callErr != nil {
			return response{}, callErr
		}
		return response{document: page.Items[0], vaultID: info.VaultID}, nil
	})
	if err != nil {
		return documentOutput{}, nil, err
	}
	document := result.document
	output := documentOutput{NodeID: document.NodeID, ContentVersionID: document.ContentVersionID,
		Path: document.Path, Name: document.Name, MediaType: document.MediaType, Size: document.Size,
		ModifiedAt: document.ModifiedAt, ActiveRenditions: document.ActiveRenditions,
		privateCache: newPrivateCache()}
	return output, documentResourceLinks(result.vaultID, []api.DocumentSummary{document}), nil
}

func getDocumentScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (documentOutput, []*sdkmcp.ResourceLink, error) {
	var input getDocumentInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return documentOutput{}, nil, err
	}
	if _, err := policy.authorize(ctx, api.OperationRead, []string{input.ContentVersionID}, true, true); err != nil {
		return documentOutput{}, nil, err
	}
	return getDocument(ctx, lease, raw)
}

type listDocumentVersionsInput struct {
	NodeID int64 `json:"node_id"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

type documentVersionOutput struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Size             int64  `json:"size"`
	MediaType        string `json:"media_type"`
	RecordedAt       string `json:"recorded_at"`
	IsCurrent        bool   `json:"is_current"`
}

type listDocumentVersionsOutput struct {
	privateCache

	NodeID int64                   `json:"node_id"`
	Items  []documentVersionOutput `json:"items"`
	Total  int                     `json:"total"`
	Limit  int                     `json:"limit"`
	Offset int                     `json:"offset"`
}

func listDocumentVersions(
	ctx context.Context, lease *daemonLease, raw []byte,
) (listDocumentVersionsOutput, error) {
	var input listDocumentVersionsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return listDocumentVersionsOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	type response struct {
		current string
		page    api.ContentVersionPage
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (response, error) {
		node, callErr := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: input.NodeID}})

		if callErr != nil {
			return response{}, callErr
		}
		if node.Kind != "file" || node.TrashedAt != "" || node.Path == "" || node.CurrentVersionID == "" {
			return response{}, store.ErrNotFound
		}
		page, callErr := c.API().ListContentVersions(ctx, &apiclient.ListContentVersionsRequestOptions{PathParams: &apiclient.ListContentVersionsPath{ID: input.NodeID}, Query: &apiclient.ListContentVersionsQuery{Limit: new(int64(input.Limit)), Offset: new(int64(input.Offset))}})

		if callErr != nil {
			return response{}, callErr
		}
		return response{current: node.CurrentVersionID, page: *page}, nil
	})
	if err != nil {
		return listDocumentVersionsOutput{}, err
	}
	if result.page.Limit != input.Limit || result.page.Offset != input.Offset || result.page.Total < 0 ||
		len(result.page.Items) > input.Limit ||
		(len(result.page.Items) == 0 && input.Offset < result.page.Total) ||
		(len(result.page.Items) > 0 && input.Offset+len(result.page.Items) > result.page.Total) {
		return listDocumentVersionsOutput{}, errors.New("document version page does not bind its request")
	}
	output := listDocumentVersionsOutput{NodeID: input.NodeID,
		Items: make([]documentVersionOutput, len(result.page.Items)), Total: result.page.Total,
		Limit: result.page.Limit, Offset: result.page.Offset, privateCache: newPrivateCache()}
	for index, version := range result.page.Items {
		if version.NodeID != input.NodeID {
			return listDocumentVersionsOutput{}, errors.New("document version response escaped its node")
		}
		output.Items[index] = documentVersionOutput{NodeID: version.NodeID,
			ContentVersionID: version.ID, Size: version.Size, MediaType: version.MimeType,
			RecordedAt: version.RecordedAt, IsCurrent: version.ID == result.current}
	}
	return output, nil
}

func listDocumentVersionsScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (listDocumentVersionsOutput, error) {
	if !policy.local() {
		// Filtering one already paged node history can disclose a misleading
		// total and skip later authorized versions. Keep this operation closed
		// until the daemon can page within the source fence itself.
		return listDocumentVersionsOutput{}, api.ErrOperationDenied
	}
	output, err := listDocumentVersions(ctx, lease, raw)
	return output, err
}

func readRenditionText(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input api.RenditionWindowRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	if input.MaxChars == 0 {
		input.MaxChars = defaultRenditionChars
	}
	window, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.RenditionTextWindow, error) {
		return c.RenditionTextWindow(ctx, input)
	})
	if err != nil {
		return nil, err
	}
	return struct {
		api.RenditionTextWindow
		privateCache
	}{window, newPrivateCache()}, nil
}

func readRenditionTextScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (any, error) {
	var input api.RenditionWindowRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	if _, err := policy.authorize(ctx, api.OperationRead, []string{input.ContentVersionID}, true, true); err != nil {
		return nil, err
	}
	return readRenditionText(ctx, lease, raw)
}

type processingPlanOutput struct {
	api.ProcessingPlan
	privateCache
}

func getProcessingPlan(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input api.ProcessingSelector
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	plan, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.ProcessingPlan, error) {
		return c.API().PlanDocumentProcessing(ctx, &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: input}})
	})
	if err != nil {
		return nil, err
	}
	if plan.Selector != input {
		return nil, errors.New("processing plan response does not bind its selector")
	}
	return processingPlanOutput{ProcessingPlan: *plan, privateCache: newPrivateCache()}, nil
}

func getProcessingPlanScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (any, error) {
	var input api.ProcessingSelector
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	if _, err := policy.authorize(ctx, api.OperationProcessing, []string{input.ContentVersionID}, true, true); err != nil {
		return nil, err
	}
	return getProcessingPlan(ctx, lease, raw)
}

func getProcessingStatus(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	status, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProcessingStatus, error) {
		return c.ProcessingStatus(ctx, input.JobID)
	})
	if err != nil {
		return nil, err
	}
	if status.JobID != input.JobID {
		return nil, errors.New("processing status response does not bind its job")
	}
	return struct {
		api.ProcessingStatus
		privateCache
	}{status, newPrivateCache()}, nil
}

func getProcessingStatusScoped(ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, raw []byte) (any, error) {
	if policy.local() {
		return getProcessingStatus(ctx, lease, raw)
	}
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	sourceID, ok := plans.jobSource(input.JobID)
	if !ok {
		return nil, api.ErrOperationNotFound
	}
	if _, err := policy.authorize(ctx, api.OperationProcessing, []string{sourceID}, true, true); err != nil {
		return nil, err
	}
	return getProcessingStatus(ctx, lease, raw)
}

type coverageClassOutput struct {
	Name                      string `json:"name"`
	Required                  bool   `json:"required"`
	State                     string `json:"state"`
	Complete                  int    `json:"complete"`
	Unavailable               int    `json:"unavailable"`
	Stale                     int    `json:"stale"`
	Ineligible                int    `json:"ineligible"`
	Rebuilding                int    `json:"rebuilding"`
	PreviousGenerationServing int    `json:"previous_generation_serving"`
	Total                     int    `json:"total"`
}

type processingCoverageOutput struct {
	privateCache

	VaultID            string                `json:"vault_id"`
	ContentVersionIDs  []string              `json:"content_version_ids"`
	ProfileFingerprint string                `json:"profile_fingerprint"`
	State              string                `json:"state"`
	Coverage           []coverageClassOutput `json:"coverage"`
}

func getProcessingCoverage(
	ctx context.Context, lease *daemonLease, raw []byte,
) (processingCoverageOutput, error) {
	var input struct {
		Profile           string   `json:"profile"`
		VaultID           string   `json:"vault_id"`
		ContentVersionIDs []string `json:"content_version_ids"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return processingCoverageOutput{}, err
	}
	vaultID, err := uuid.Parse(input.VaultID)
	if err != nil {
		return processingCoverageOutput{}, fmt.Errorf("parsing coverage vault ID: %w", err)
	}
	report, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.CoverageReport, error) {
		return c.API().GetDocumentProcessingCoverage(ctx, &apiclient.GetDocumentProcessingCoverageRequestOptions{Query: &apiclient.GetDocumentProcessingCoverageQuery{
			Profile: input.Profile, VaultUID: vaultID, ContentVersionID: input.ContentVersionIDs}})
	})
	if err != nil {
		return processingCoverageOutput{}, err
	}
	if report.VaultUID != input.VaultID {
		return processingCoverageOutput{}, errors.New("processing coverage escaped its vault fence")
	}
	classes := append([]api.CoverageClass{report.Renditions}, report.Embeddings...)
	output := processingCoverageOutput{VaultID: report.VaultUID,
		ContentVersionIDs:  slices.Clone(input.ContentVersionIDs),
		ProfileFingerprint: report.ProfileFingerprint, State: report.State,
		Coverage: make([]coverageClassOutput, len(classes)), privateCache: newPrivateCache()}
	for index, item := range classes {
		output.Coverage[index] = coverageClassOutput{Name: item.Name, Required: item.Required,
			State: item.State, Complete: item.Complete, Unavailable: item.Unavailable,
			Stale: item.Stale, Ineligible: item.Ineligible, Rebuilding: item.Rebuilding,
			PreviousGenerationServing: item.PreviousGenerationServing, Total: item.Total}
	}
	return output, nil
}

func getProcessingCoverageScoped(ctx context.Context, lease *daemonLease, policy operationPolicy, raw []byte) (processingCoverageOutput, error) {
	var input struct {
		Profile           string   `json:"profile"`
		VaultID           string   `json:"vault_id"`
		ContentVersionIDs []string `json:"content_version_ids"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return processingCoverageOutput{}, err
	}
	decision, err := policy.authorize(ctx, api.OperationAnalyze, input.ContentVersionIDs, false, false)
	if err != nil {
		return processingCoverageOutput{}, err
	}
	input.ContentVersionIDs = decision.SourceIDs
	rewritten, err := json.Marshal(input)
	if err != nil {
		return processingCoverageOutput{}, err
	}
	return getProcessingCoverage(ctx, lease, rewritten)
}

func filterResourceLinks(links []*sdkmcp.ResourceLink, allowed map[string]bool) []*sdkmcp.ResourceLink {
	filtered := links[:0]
	for _, link := range links {
		identity, _, err := parseRenditionResourceURI(link.URI)
		if err == nil && allowed[identity.ContentVersionID] {
			filtered = append(filtered, link)
		}
	}
	return filtered
}

func documentResourceLinks(vaultID string, documents []api.DocumentSummary) []*sdkmcp.ResourceLink {
	links := make([]*sdkmcp.ResourceLink, 0)
	for _, document := range documents {
		for _, rendition := range document.ActiveRenditions {
			uri := renditionResourceURI(renditionResourceIdentity{VaultID: vaultID, NodeID: document.NodeID,
				ContentVersionID: document.ContentVersionID, AttachmentID: rendition.AttachmentID})
			links = append(links, &sdkmcp.ResourceLink{URI: uri,
				Name:  "docbank-rendition-" + rendition.AttachmentID,
				Title: document.Name + " rendition", MIMEType: "text/markdown"})
		}
	}
	return links
}

func boundedToolSuccess(
	validator *jsonschema.Resolved, output any, links []*sdkmcp.ResourceLink,
) (*sdkmcp.CallToolResult, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, errors.New("daemon result is not valid JSON")
	}
	if validator == nil || validator.Validate(&normalized) != nil {
		return nil, errors.New("daemon result does not conform to the published tool schema")
	}
	content := make([]sdkmcp.Content, 0, 1+len(links))
	content = append(content, &sdkmcp.TextContent{Text: string(encoded)})
	for _, link := range links {
		content = append(content, link)
	}
	result := &sdkmcp.CallToolResult{Content: content, StructuredContent: normalized}
	complete, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(complete) > maxToolResponseBytes {
		return nil, fmt.Errorf("%w: %d bytes", errToolResultTooLarge, len(complete))
	}
	return result, nil
}
