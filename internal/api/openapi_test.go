package api_test

import (
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestOpenAPIDocumentOffline(t *testing.T) {
	// No store, no blobs, no listener: registration must not touch deps.
	out, err := api.OpenAPIYAML()
	require.NoError(t, err)
	doc := string(out)
	for _, op := range []string{"getNode", "resolvePath", "listChildren", "getNodeContent", "verifyNodeContent",
		"listContentVersions", "getContentVersion", "getContentVersionBytes", "pruneNodeContentVersions",
		"lookupContentReferences",
		"listCollections", "getCollection", "listCollectionMembers", "getCollectionLabel", "setCollectionLabel",
		"listTags", "resolveTagByName", "getTag", "listTagNodes", "listNodeTags",
		"createTag", "renameTag", "deleteTag", "assignTag", "unassignTag",
		"assignTagPath", "unassignTagPath",
		"listSavedQueries", "createSavedQuery", "getSavedQuery", "updateSavedQuery", "deleteSavedQuery",
		"previewAuditEnrollment", "enableAudit", "auditStatus", "auditNodeHistory", "verifyAudit",
		"search", "createNode", "moveNode", "movePath", "trashNode", "trashPath", "restoreNode",
		"storageStatus", "storagePack", "storageRepack", "ingest", "uploadFile", "listTrash", "emptyTrash", "gc", "verify", "appendNodeProvenance",
		"initBackupRepository", "createBackupSnapshot", "listBackupSnapshots", "listJobs"} {
		assert.Contains(t, doc, op, "operation missing from OpenAPI doc")
	}
	assert.NotContains(t, doc, "/api/daemon/shutdown", "lifecycle plumbing must stay hidden")
	assert.NotContains(t, doc, "/api/daemon/challenge", "lifecycle plumbing must stay hidden")
	assert.Contains(t, doc, "X-Docbank-Blob-Hash")
	assert.Contains(t, doc, api.ContentVersionHeader)
	assert.Contains(t, doc, "Content-Digest")
	assert.Contains(t, doc, "computed_hash")
	for _, schema := range []string{"IngestPreflightRequest", "IngestRequest"} {
		block := openAPISchemaBlock(t, doc, schema)
		assert.Contains(t, block, "        include:")
		assert.Contains(t, block, "        exclude:")
		if schema == "IngestRequest" {
			assert.Contains(t, block, "        dest:")
		} else {
			assert.NotContains(t, block, "        dest:")
		}
	}
}

func TestOpenAPISavedQueriesAreStructuredAndRevisionFenced(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	collection := doc.Paths["/api/v1/saved-queries"]
	require.NotNil(t, collection.Get)
	require.NotNil(t, collection.Post)
	item := doc.Paths["/api/v1/saved-queries/{saved_query_id}"]
	require.NotNil(t, item.Get)
	require.NotNil(t, item.Patch)
	require.NotNil(t, item.Delete)
	assert.Nil(t, item.Post)
	assert.Nil(t, item.Put)
	assert.Nil(t, doc.Paths["/api/v1/saved-queries/{saved_query_id}/run"])

	for _, operation := range []*huma.Operation{item.Patch, item.Delete} {
		required := map[string]bool{}
		for _, parameter := range operation.Parameters {
			required[parameter.Name] = parameter.Required
		}
		assert.True(t, required["If-Match"])
	}
	require.NotNil(t, item.Patch.RequestBody)
	patchBody := resolveOpenAPISchema(t, doc.Components.Schemas.Map(),
		item.Patch.RequestBody.Content["application/json"].Schema)
	assert.Contains(t, patchBody.Properties["name"].Description,
		"256 UTF-8 bytes after NFC normalization")

	schemas := doc.Components.Schemas.Map()
	create := schemas["SavedQueryCreateRequest"]
	require.NotNil(t, create)
	assert.ElementsMatch(t, []string{"kind", "name", "payload"}, create.Required)
	assert.Nil(t, create.Properties["name"].MaxLength)
	assert.Contains(t, create.Properties["name"].Description, "256 UTF-8 bytes after NFC normalization")
	payload := resolveOpenAPISchema(t, schemas, create.Properties["payload"])
	assert.Empty(t, payload.Format)
	require.Len(t, payload.OneOf, 2)
	variants := map[string]*huma.Schema{}
	for _, variant := range payload.OneOf {
		resolved := resolveOpenAPISchema(t, schemas, variant)
		if _, ok := resolved.Properties["terms"]; ok {
			variants["highlight"] = resolved
		}
		if _, ok := resolved.Properties["filters"]; ok {
			variants["query"] = resolved
		}
	}
	require.Contains(t, variants, "query")
	require.Contains(t, variants, "highlight")
	querySchema := variants["query"]
	for _, field := range []string{"v", "text", "syntax", "mode", "filters", "sort"} {
		assert.Contains(t, querySchema.Properties, field)
	}
	assert.Equal(t, 1, querySchema.Properties["v"].Default)
	assert.Equal(t, "simple", querySchema.Properties["syntax"].Default)
	assert.Equal(t, "lexical", querySchema.Properties["mode"].Default)
	filters := resolveOpenAPISchema(t, schemas, querySchema.Properties["filters"])
	for _, field := range []struct {
		name     string
		maxItems int
	}{
		{"paths", 64},
		{"exclude_paths", 64},
		{"collection_ids", 64},
		{"exclude_collection_ids", 64},
		{"tag_ids", 64},
		{"exclude_tag_ids", 64},
		{"media_families", 13},
		{"mime_types", 64},
		{"extensions", 32},
		{"text_coverage", 6},
	} {
		property := resolveOpenAPISchema(t, schemas, filters.Properties[field.name])
		assert.False(t, property.UniqueItems, field.name)
		require.NotNil(t, property.MaxItems, field.name)
		assert.Equal(t, field.maxItems, *property.MaxItems, field.name)
	}
	for _, field := range []string{
		"no_tags", "modified_after", "modified_before", "size_min", "size_max",
		"has_duplicates", "collapse_duplicates",
	} {
		assert.True(t, filters.Properties[field].Nullable, field)
		assert.NotContains(t, filters.Required, field)
	}
	for _, field := range []string{
		"collection_ids", "exclude_collection_ids", "tag_ids", "exclude_tag_ids",
	} {
		require.NotNil(t, filters.Properties[field].Items)
		assert.Equal(t, "uuid", filters.Properties[field].Items.Format)
	}
	require.NotNil(t, filters.Properties["extensions"].Items)
	assert.Equal(t, "^[a-z0-9](?:[a-z0-9_-]{0,31})$",
		filters.Properties["extensions"].Items.Pattern)
	mediaFamilies := resolveOpenAPISchema(t, schemas, filters.Properties["media_families"])
	require.NotNil(t, mediaFamilies.Items)
	assert.ElementsMatch(t, []any{
		"email", "document", "spreadsheet", "presentation", "image", "audio_video",
		"text", "source_code", "web", "calendar", "archive", "cad", "unknown",
	}, mediaFamilies.Items.Enum)
	highlightSchema := variants["highlight"]
	assert.ElementsMatch(t, []string{"terms"}, highlightSchema.Required)
	assert.Equal(t, 1, highlightSchema.Properties["v"].Default)
	require.NotNil(t, highlightSchema.Properties["terms"].MaxItems)
	assert.Equal(t, 64, *highlightSchema.Properties["terms"].MaxItems)

	record := schemas["SavedQuery"]
	require.NotNil(t, record)
	for _, field := range []string{
		"id", "name", "description", "kind", "payload", "fingerprint",
		"revision", "created_at", "updated_at",
	} {
		assert.Contains(t, record.Properties, field)
	}
	require.NotNil(t, record.Properties["name"].MaxLength)
	assert.Equal(t, 256, *record.Properties["name"].MaxLength)
}

func TestOpenAPIWorkspaceSnapshotsExposeStrictBoundedAuthority(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	create := doc.Paths["/api/v1/workspace/queries"].Post
	page := doc.Paths["/api/v1/workspace/queries/{id}/pages"].Post
	run := doc.Paths["/api/v1/saved-queries/{saved_query_id}/runs"].Post
	require.NotNil(t, create)
	require.NotNil(t, page)
	require.NotNil(t, run)
	assert.Equal(t, "createWorkspaceQuery", create.OperationID)
	assert.Equal(t, "readWorkspaceQueryPage", page.OperationID)
	assert.Equal(t, "runSavedQuery", run.OperationID)
	required := map[string]bool{}
	for _, parameter := range run.Parameters {
		required[parameter.Name] = parameter.Required
	}
	assert.True(t, required["If-Match"])

	schemas := doc.Components.Schemas.Map()
	request := schemas["WorkspaceQueryCreateRequest"]
	require.NotNil(t, request)
	assert.Contains(t, request.Required, "query")
	assert.Equal(t, 100, request.Properties["page_size"].Default)
	queryBody := resolveOpenAPISchema(t, schemas, request.Properties["query"])
	assert.Contains(t, queryBody.Properties, "filters")
	assert.NotContains(t, queryBody.Properties, "terms", "workspace creation accepts QueryV1, not highlight payloads")

	response := schemas["WorkspaceQueryResponse"]
	require.NotNil(t, response)
	rows := resolveOpenAPISchema(t, schemas, response.Properties["rows"])
	require.NotNil(t, rows.MaxItems)
	assert.Equal(t, 250, *rows.MaxItems)
	dependency := schemas["WorkspaceQueryDependency"]
	require.NotNil(t, dependency)
	assert.ElementsMatch(t, []string{"id", "kind", "revision"}, dependency.Required)
	for _, field := range []string{"Kind", "ID", "Revision"} {
		assert.NotContains(t, dependency.Properties, field)
	}

	facets := resolveOpenAPISchema(t, schemas, response.Properties["facets"])
	facet := resolveOpenAPISchema(t, schemas, facets.Items)
	values := resolveOpenAPISchema(t, schemas, facet.Properties["values"])
	require.NotNil(t, values.MaxItems)
	assert.Equal(t, 114, *values.MaxItems)
	for _, field := range []string{"total", "missing", "other"} {
		require.Len(t, facet.Properties[field].AnyOf, 2, field)
		assert.Equal(t, "null", facet.Properties[field].AnyOf[1].Type, field)
		assert.Nil(t, facet.Properties[field].Default, field)
	}
}

func resolveOpenAPISchema(
	t *testing.T, schemas map[string]*huma.Schema, schema *huma.Schema,
) *huma.Schema {
	t.Helper()
	require.NotNil(t, schema)
	if schema.Ref == "" {
		return schema
	}
	const prefix = "#/components/schemas/"
	require.True(t, strings.HasPrefix(schema.Ref, prefix), schema.Ref)
	resolved := schemas[strings.TrimPrefix(schema.Ref, prefix)]
	require.NotNil(t, resolved, schema.Ref)
	return resolved
}

func openAPISchemaBlock(t *testing.T, doc, schema string) string {
	t.Helper()
	marker := "\n    " + schema + ":\n"
	start := strings.Index(doc, marker)
	require.NotEqual(t, -1, start, "schema missing from OpenAPI document")
	block := doc[start+len(marker):]
	for offset := strings.Index(block, "\n"); offset >= 0; {
		line := block[offset+1:]
		if line != "" && !strings.HasPrefix(line, "      ") {
			block = block[:offset]
			break
		}
		if strings.HasPrefix(line, "    ") && len(line) > 4 && line[4] != ' ' {
			block = block[:offset]
			break
		}
		next := strings.Index(line, "\n")
		if next < 0 {
			break
		}
		offset += next + 1
	}
	return block
}

func TestOpenAPIDeclaresSecurity(t *testing.T) {
	// Generated clients must learn from the document alone that every
	// operation needs credentials (X-Api-Key header or bearer token).
	out, err := api.OpenAPIYAML()
	require.NoError(t, err)
	doc := string(out)
	assert.Contains(t, doc, "securitySchemes")
	assert.Contains(t, doc, "X-Api-Key")
	assert.Contains(t, doc, "scheme: bearer")
	assert.Contains(t, doc, "security:", "document-level security requirement missing")
}

func TestOpenAPISearchQueryIsOptional(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	op := doc.Paths["/api/v1/search"].Get
	require.NotNil(t, op)
	for _, param := range op.Parameters {
		if param.Name == "q" {
			assert.False(t, param.Required)
			return
		}
	}
	t.Fatal("search q parameter missing")
}

func TestLongRunningBackupRoutesClearBodyReadDeadline(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	for _, operation := range []*huma.Operation{
		doc.Paths["/api/v1/backup/snapshots"].Post,
		doc.Paths["/api/v1/backup/snapshots/stream"].Post,
		doc.Paths["/api/v1/backup/verify"].Post,
		doc.Paths["/api/v1/backup/verify/stream"].Post,
		doc.Paths["/api/v1/backup/restore"].Post,
		doc.Paths["/api/v1/backup/restore/stream"].Post,
	} {
		require.NotNil(t, operation)
		assert.Negative(t, operation.BodyReadTimeout)
	}
}

func TestOpenAPIAuditEnableDisclosesCompleteRetention(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	op := doc.Paths["/api/v1/audit/enable"].Post
	require.NotNil(t, op)
	for _, class := range []string{"names", "topology", "tags", "assignments", "ingests", "provenance"} {
		assert.Contains(t, op.Description, class)
	}
}

func TestOpenAPIDeclaresDigestCheckedUpload(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	op := doc.Paths["/api/v1/uploads"].Post
	require.NotNil(t, op)
	assert.Equal(t, "uploadFile", op.OperationID)
	form := op.RequestBody.Content["multipart/form-data"]
	require.NotNil(t, form)
	assert.Equal(t, "binary", form.Schema.Properties["file"].Format)
	assert.Contains(t, form.Schema.Required, "file")

	required := map[string]bool{}
	for _, param := range op.Parameters {
		required[param.Name] = param.Required
	}
	for _, name := range []string{"parent_id", "name", api.BlobHashHeader, api.BlobSizeHeader} {
		assert.True(t, required[name], "%s must be required", name)
	}
	assert.NotNil(t, op.Responses["200"])
	assert.NotNil(t, op.Responses["201"])

	replace := doc.Paths["/api/v1/nodes/{id}/content"].Put
	require.NotNil(t, replace)
	require.NotNil(t, replace.RequestBody)
	assert.Contains(t, replace.RequestBody.Content, "*/*")
	assert.NotNil(t, replace.Responses["200"])

	revert := doc.Paths["/api/v1/nodes/{id}/revert"].Post
	require.NotNil(t, revert)
	require.NotNil(t, revert.RequestBody)
	assert.Contains(t, revert.RequestBody.Content, "application/json")
	assert.NotNil(t, revert.Responses["200"])
}

func TestOpenAPIDeclaresMutationPreconditions(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	pruneRequest := doc.Components.Schemas.Map()["VersionPruneRequest"]
	require.NotNil(t, pruneRequest)
	versionIDs := pruneRequest.Properties["version_ids"]
	require.NotNil(t, versionIDs)
	require.NotNil(t, versionIDs.MaxItems)
	assert.Equal(t, 1000, *versionIDs.MaxItems)
	assert.True(t, versionIDs.UniqueItems)
	require.NotNil(t, versionIDs.Items)
	assert.Equal(t, "uuid", versionIDs.Items.Format)
	keepNewest := pruneRequest.Properties["keep_newest"]
	require.NotNil(t, keepNewest)
	require.NotNil(t, keepNewest.Minimum)
	assert.InDelta(t, 1, *keepNewest.Minimum, 0)
	for _, operation := range []*huma.Operation{
		doc.Paths["/api/v1/collections/{id}/label"].Put,
		doc.Paths["/api/v1/nodes/{id}"].Patch,
		doc.Paths["/api/v1/nodes/{id}/trash"].Post,
		doc.Paths["/api/v1/nodes/{id}/restore"].Post,
		doc.Paths["/api/v1/nodes/{id}/verify"].Post,
		doc.Paths["/api/v1/nodes/{id}/revert"].Post,
		doc.Paths["/api/v1/nodes/{id}/versions/prune"].Post,
		doc.Paths["/api/v1/nodes/{id}/provenance"].Post,
		doc.Paths["/api/v1/nodes/{id}/tags/{tag_id}"].Put,
		doc.Paths["/api/v1/nodes/{id}/tags/{tag_id}"].Delete,
		doc.Paths["/api/v1/tags/{tag_id}"].Patch,
		doc.Paths["/api/v1/tags/{tag_id}"].Delete,
	} {
		require.NotNil(t, operation)
		required := map[string]bool{}
		for _, parameter := range operation.Parameters {
			required[parameter.Name] = parameter.Required
		}
		assert.True(t, required["If-Match"])
	}
	create := doc.Paths["/api/v1/tags"].Post
	require.NotNil(t, create)
	assert.NotNil(t, create.Responses["201"])
}

func TestOpenAPICollectionContractIsBoundedAndLabelSpecific(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	list := doc.Paths["/api/v1/collections"].Get
	members := doc.Paths["/api/v1/collections/{id}/members"].Get
	for _, operation := range []*huma.Operation{list, members} {
		require.NotNil(t, operation)
		parameters := map[string]*huma.Schema{}
		for _, parameter := range operation.Parameters {
			parameters[parameter.Name] = parameter.Schema
		}
		require.NotNil(t, parameters["limit"])
		require.NotNil(t, parameters["limit"].Minimum)
		require.NotNil(t, parameters["limit"].Maximum)
		assert.InDelta(t, 1, *parameters["limit"].Minimum, 0)
		assert.InDelta(t, 1000, *parameters["limit"].Maximum, 0)
		require.NotNil(t, parameters["offset"])
		require.NotNil(t, parameters["offset"].Minimum)
		assert.InDelta(t, 0, *parameters["offset"].Minimum, 0)
	}

	schemas := doc.Components.Schemas.Map()
	collection := schemas["Collection"]
	require.NotNil(t, collection)
	for _, field := range []string{
		"id", "source_kind", "source_description", "started_at", "file_count",
		"total_bytes", "label", "label_revision", "label_updated_at",
	} {
		assert.Contains(t, collection.Properties, field)
	}
	assert.NotContains(t, collection.Properties, "quality")
	assert.Contains(t, collection.Properties, "coverage")
	assert.Contains(t, collection.Required, "coverage")
	quality := doc.Paths["/api/v1/collections/{id}/quality"].Get
	require.NotNil(t, quality)
	coverage := collection.Properties["coverage"]
	require.NotNil(t, coverage)
	assert.ElementsMatch(t, []any{"configured", "unconfigured", "profile_required"}, coverage.Properties["configuration"].Enum)
	require.NotNil(t, coverage.Properties["counts"])
	require.Len(t, coverage.Properties["counts"].AnyOf, 2)
	assert.Equal(t, "#/components/schemas/CoverageCounts", coverage.Properties["counts"].AnyOf[0].Ref)
	assert.Equal(t, "null", coverage.Properties["counts"].AnyOf[1].Type)
	qualitySchema := schemas["CollectionQuality"]
	require.NotNil(t, qualitySchema)
	require.NotNil(t, qualitySchema.Properties["dimensions"].MaxItems)
	assert.Equal(t, 7, *qualitySchema.Properties["dimensions"].MaxItems)

	request := schemas["SetCollectionLabelRequest"]
	require.NotNil(t, request)
	assert.Equal(t, []string{"label"}, request.Required)
	assert.Len(t, request.Properties, 2, "only label plus Huma's read-only $schema member")
	assert.NotContains(t, request.Properties, "revision")
	assert.NotContains(t, request.Properties, "ingest_id")
	assert.NotContains(t, request.Properties, "updated_at")
	label := request.Properties["label"]
	require.NotNil(t, label)
	assert.True(t, label.Nullable)

	operation := doc.Paths["/api/v1/collections/{id}/label"].Put
	require.NotNil(t, operation)
	require.NotNil(t, operation.Responses["200"])
	assert.Contains(t, operation.Responses["200"].Headers, "ETag")
}

func TestOpenAPIProvenanceMTimeUsesDateTimeFormat(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	for _, name := range []string{"ProvenanceAppendRequest", "ProvenanceFact"} {
		schema := doc.Components.Schemas.Map()[name]
		require.NotNil(t, schema)
		mtime := schema.Properties["original_mtime"]
		require.NotNil(t, mtime)
		assert.Equal(t, "date-time", mtime.Format)
	}
}
