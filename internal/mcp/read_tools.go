package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var errToolResultTooLarge = errors.New("MCP tool result exceeds the response limit")

type privateCache struct {
	TTLMs      int    `json:"ttlMs"`
	CacheScope string `json:"cacheScope"`
}

func newPrivateCache() privateCache { return privateCache{CacheScope: "private"} }

func readToolHandler(lease *daemonLease, plans *processingPlanRegistry, name string) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executeReadTool(ctx, lease, plans, name, request.Params.Arguments)
		if err != nil {
			return nil, sanitizedRPCError(err)
		}
		return result, nil
	}
}

func invokeReadTool(
	ctx context.Context, lease *daemonLease, name string, arguments map[string]any,
) (*sdkmcp.CallToolResult, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	return executeReadTool(ctx, lease, newProcessingPlanRegistry(), name, raw)
}

func executeReadTool(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, name string, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	var output any
	var links []*sdkmcp.ResourceLink
	var err error
	switch name {
	case "get_vault_info":
		output, err = getVaultInfo(ctx, lease, raw)
	case "list_documents":
		output, links, err = listDocuments(ctx, lease, raw)
	case "search_documents":
		output, links, err = searchDocuments(ctx, lease, raw)
	case "get_document":
		output, links, err = getDocument(ctx, lease, raw)
	case "list_document_versions":
		output, err = listDocumentVersions(ctx, lease, raw)
	case "read_rendition_text":
		output, err = readRenditionText(ctx, lease, raw)
	case "get_processing_plan":
		output, err = getProcessingPlan(ctx, lease, raw)
	case "get_processing_status":
		output, err = getProcessingStatus(ctx, lease, raw)
	case "get_processing_coverage":
		output, err = getProcessingCoverage(ctx, lease, raw)
	default:
		return nil, errors.New("unknown Docbank read tool")
	}
	if err != nil {
		if domain, ok := domainToolError(err); ok {
			return domain, nil
		}
		return nil, err
	}
	result, err := boundedToolSuccess(name, output, links)
	if err == nil && name == "get_processing_plan" {
		plan, ok := output.(processingPlanOutput)
		if !ok {
			return nil, errors.New("processing plan result has an invalid internal type")
		}
		plans.remember(plan.ProcessingPlan)
	}
	return result, err
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
	info, err := daemonRead(ctx, lease, func(c *client.Client) (api.VaultInfo, error) { return c.Info(ctx) })
	if err != nil {
		return vaultInfoOutput{}, err
	}
	return vaultInfoOutput{VaultID: info.VaultID, LiveFiles: info.LiveFiles,
		LiveDirectories: info.LiveDirectories, TrashedNodes: info.TrashedNodes,
		ContentVersions: info.ContentVersions, LogicalVersionBytes: info.LogicalVersionBytes,
		TrackedBlobs: info.TrackedBlobs, TrackedBlobBytes: info.TrackedBlobBytes,
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
	result, err := daemonRead(ctx, lease, func(c *client.Client) (response, error) {
		page, callErr := c.ListDocuments(ctx, api.DocumentQuery{PathPrefix: input.PathPrefix,
			Sort: input.Sort, Direction: input.Direction, PageSize: input.PageSize, Cursor: input.Cursor})
		if callErr != nil {
			return response{}, callErr
		}
		info, callErr := c.Info(ctx)
		return response{page: page, info: info}, callErr
	})
	if err != nil {
		return listDocumentsOutput{}, nil, err
	}
	return listDocumentsOutput{DocumentPage: result.page, privateCache: newPrivateCache()},
		documentResourceLinks(result.info.VaultID, result.page.Items), nil
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
	ctx context.Context, lease *daemonLease, raw []byte,
) (searchDocumentsOutput, []*sdkmcp.ResourceLink, error) {
	var input searchDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return searchDocumentsOutput{}, nil, err
	}
	type response struct {
		resolution api.DocumentSourceFenceResolution
		report     api.DocumentSearchReport
		documents  []api.DocumentSummary
	}
	result, err := daemonRead(ctx, lease, func(c *client.Client) (response, error) {
		resolution, callErr := c.ResolveDocumentSourceFence(ctx, api.DocumentSourceFenceResolveRequest{
			ContentVersionIDs: input.ContentVersionIDs, Filters: input.Filters})
		if callErr != nil {
			return response{}, callErr
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
	result, err := daemonRead(ctx, lease, func(c *client.Client) (response, error) {
		node, callErr := c.Node(ctx, input.NodeID)
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
		info, callErr := c.Info(ctx)
		return response{document: page.Items[0], vaultID: info.VaultID}, callErr
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
	result, err := daemonRead(ctx, lease, func(c *client.Client) (response, error) {
		node, callErr := c.Node(ctx, input.NodeID)
		if callErr != nil {
			return response{}, callErr
		}
		if node.Kind != "file" || node.TrashedAt != "" || node.Path == "" || node.CurrentVersionID == "" {
			return response{}, store.ErrNotFound
		}
		page, callErr := c.Versions(ctx, input.NodeID, input.Limit, input.Offset)
		return response{current: node.CurrentVersionID, page: page}, callErr
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

func readRenditionText(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input api.RenditionWindowRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	if input.MaxChars == 0 {
		input.MaxChars = defaultRenditionChars
	}
	window, err := daemonRead(ctx, lease, func(c *client.Client) (api.RenditionTextWindow, error) {
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

type processingPlanOutput struct {
	api.ProcessingPlan
	privateCache
}

func getProcessingPlan(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input api.ProcessingSelector
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	plan, err := daemonRead(ctx, lease, func(c *client.Client) (api.ProcessingPlan, error) {
		return c.PlanProcessing(ctx, api.ProcessingPlanRequest{Selector: input})
	})
	if err != nil {
		return nil, err
	}
	if plan.Selector != input {
		return nil, errors.New("processing plan response does not bind its selector")
	}
	return processingPlanOutput{ProcessingPlan: plan, privateCache: newPrivateCache()}, nil
}

func getProcessingStatus(ctx context.Context, lease *daemonLease, raw []byte) (any, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	status, err := daemonRead(ctx, lease, func(c *client.Client) (api.ProcessingStatus, error) {
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
	report, err := daemonRead(ctx, lease, func(c *client.Client) (api.CoverageReport, error) {
		return c.DocumentCoverage(ctx, input.Profile, api.DocumentSourceFence{
			VaultUID: input.VaultID, ContentVersionIDs: input.ContentVersionIDs})
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
	name string, output any, links []*sdkmcp.ResourceLink,
) (*sdkmcp.CallToolResult, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, errors.New("daemon result is not valid JSON")
	}
	var outputSchema schema
	definitions := append(append([]toolDefinition{}, readToolDefinitions...), processingToolDefinition)
	for _, definition := range definitions {
		if definition.name == name {
			_, outputSchema = definition.schemas()
			break
		}
	}
	if outputSchema == nil || mustResolveSchema(outputSchema).Validate(&normalized) != nil {
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
