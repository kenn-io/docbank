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
		"rebuildDocumentPeople", "getPeopleRebuild", "getPeopleCoverage",
		"getEmailMetadata", "ensureEmailMetadata", "getEmailMetadataGeneration", "getEmailPart",
		"listTags", "resolveTagByName", "getTag", "listTagNodes", "listNodeTags",
		"createTag", "renameTag", "deleteTag", "assignTag", "unassignTag",
		"assignTagPath", "unassignTagPath",
		"listSavedQueries", "createSavedQuery", "getSavedQuery", "updateSavedQuery", "deleteSavedQuery",
		"previewAuditEnrollment", "enableAudit", "auditStatus", "auditNodeHistory", "verifyAudit",
		"listDocumentProcessingProfiles", "planDocumentProcessing", "startDocumentProcessing",
		"getDocumentProcessingJob", "grantDocumentProcessingConsent", "revokeDocumentProcessingConsent",
		"planDerivativePurge", "runDerivativePurge", "getDocumentRendition",
		"getDocumentProcessingCoverage", "validateDocumentSearch", "searchDocuments",
		"search", "createNode", "moveNode", "movePath", "trashNode", "trashPath", "restoreNode",
		"storageStatus", "storagePack", "storageRepack", "ingest", "uploadFile", "listTrash", "emptyTrash", "gc", "verify", "appendNodeProvenance",
		"createPhotoAsset", "getPhotoAsset", "getPhotoAssetByNode", "attachPhotoFile", "detachPhotoFile",
		"excludePhotoAsset", "promotePhotoNode", "setPhotoDisplay", "getPhotoSettings", "setPhotoSettings",
		"initBackupRepository", "createBackupSnapshot", "listBackupSnapshots", "listJobs"} {
		assert.Contains(t, doc, op, "operation missing from OpenAPI doc")
	}
	assert.Contains(t, doc, "/api/daemon/shutdown", "offline clients need lifecycle operations")
	assert.Contains(t, doc, "/api/daemon/challenge", "offline clients need lifecycle operations")
	assert.Contains(t, doc, "X-Docbank-Blob-Hash")
	assert.Contains(t, doc, api.ContentVersionHeader)
	assert.Contains(t, doc, "Content-Digest")
	assert.Contains(t, doc, "computed_hash")
	assert.Contains(t, doc, "rerank")
	assert.Contains(t, doc, "DocumentSearchRerankingReceipt")
	assert.Contains(t, doc, "candidate_count")
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

func TestOpenAPIDeclaresEmailMetadataAndBinaryParts(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	metadata := doc.Paths["/api/v1/versions/{version_id}/email"]
	require.NotNil(t, metadata)
	require.NotNil(t, metadata.Get)
	require.NotNil(t, metadata.Post)
	assert.NotNil(t, metadata.Get.Responses["200"])
	assert.NotNil(t, metadata.Get.Responses["202"])
	request := metadata.Post.RequestBody.Content["application/json"]
	require.NotNil(t, request)
	assert.Equal(t, "object", request.Schema.Type)
	assert.Equal(t, false, request.Schema.AdditionalProperties)

	generation := doc.Paths["/api/v1/versions/{version_id}/email/generations/{generation_id}"]
	require.NotNil(t, generation)
	require.NotNil(t, generation.Get)
	part := doc.Paths["/api/v1/versions/{version_id}/email/generations/{generation_id}/parts/{part_path}/{role}"]
	require.NotNil(t, part)
	require.NotNil(t, part.Get)
	success := part.Get.Responses["200"]
	require.NotNil(t, success)
	binary := success.Content["application/octet-stream"]
	require.NotNil(t, binary)
	assert.Equal(t, "binary", binary.Schema.Format)
	for _, header := range []string{
		api.ContentVersionHeader, api.EmailGenerationHeader, api.EmailAttachmentHeader,
		api.EmailPartPathHeader, api.EmailPartRoleHeader, api.BlobHashHeader,
		api.BlobSizeHeader, "Content-Digest",
	} {
		assert.Contains(t, success.Headers, header)
	}
}

func TestOpenAPIDeclaresPackagePreflightContract(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	schemas := doc.Components.Schemas.Map()
	create := doc.Paths["/api/v1/packages/preflights"].Post
	read := doc.Paths["/api/v1/packages/preflights/{preflight_id}"].Get
	diagnostics := doc.Paths["/api/v1/packages/preflights/{preflight_id}/diagnostics"].Get
	require.NotNil(t, create.RequestBody)
	assert.True(t, create.RequestBody.Required)
	request := resolveOpenAPISchema(t, schemas, create.RequestBody.Content["application/json"].Schema)
	assert.ElementsMatch(t, []string{"profile", "encoding", "source_kind", "source_ref"}, request.Required)
	assert.Contains(t, request.Properties, "page_map_profile")
	assert.Contains(t, request.Properties, "mapping")
	assert.Equal(t, false, request.AdditionalProperties)
	for _, operation := range []*huma.Operation{create, read, diagnostics} {
		require.NotNil(t, operation.Responses["200"])
		body := resolveOpenAPISchema(t, schemas, operation.Responses["200"].Content["application/json"].Schema)
		assert.Contains(t, body.Properties, "diagnostics")
		if operation == diagnostics {
			assert.Contains(t, body.Properties, "total")
			assert.Contains(t, body.Properties, "next_cursor")
		} else {
			assert.Equal(t, "uuid", body.Properties["preflight_id"].Format)
			assert.Contains(t, body.Properties, "manifest_sha256")
			assert.Equal(t, "date-time", body.Properties["expires_at"].Format)
		}
		for _, status := range []string{"401", "403", "500"} {
			require.NotNil(t, operation.Responses[status])
			errorSchema := resolveOpenAPISchema(t, schemas, operation.Responses[status].Content["application/problem+json"].Schema)
			assert.Contains(t, errorSchema.Properties, "code")
			assert.Contains(t, errorSchema.Properties, "status")
		}
	}
	for _, status := range []string{"413", "422", "503"} {
		assert.Contains(t, create.Responses, status)
	}
	for _, operation := range []*huma.Operation{read, diagnostics} {
		assert.Contains(t, operation.Responses, "404")
		parameters := map[string]*huma.Param{}
		for _, parameter := range operation.Parameters {
			parameters[parameter.Name] = parameter
		}
		require.Contains(t, parameters, "preflight_id")
		assert.Equal(t, "path", parameters["preflight_id"].In)
		assert.True(t, parameters["preflight_id"].Required)
		assert.Equal(t, "uuid", parameters["preflight_id"].Schema.Format)
		if operation == diagnostics {
			require.Contains(t, parameters, "limit")
			assert.Equal(t, "query", parameters["limit"].In)
			assert.Equal(t, "integer", parameters["limit"].Schema.Type)
			assert.Equal(t, 100, parameters["limit"].Schema.Default)
			assert.Equal(t, new(float64(1)), parameters["limit"].Schema.Minimum)
			assert.Equal(t, new(float64(250)), parameters["limit"].Schema.Maximum)
			require.Contains(t, parameters, "cursor")
			assert.Equal(t, "query", parameters["cursor"].In)
			assert.Equal(t, "string", parameters["cursor"].Schema.Type)
			assert.Contains(t, operation.Responses, "422")
		}
	}
}

func TestOpenAPIDeclaresPackageContainerContract(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	require.NotNil(t, doc.Paths["/api/v1/packages/containers"].Post)
	require.NotNil(t, doc.Paths["/api/v1/packages/containers/{id}"].Get)
	require.NotNil(t, doc.Paths["/api/v1/packages/containers/{id}"].Delete)
	require.NotNil(t, doc.Paths["/api/v1/packages/containers/{id}/chunks/{index}"].Put)
	require.Negative(t, doc.Paths["/api/v1/packages/containers/{id}/chunks/{index}"].Put.BodyReadTimeout)
	require.NotNil(t, doc.Paths["/api/v1/packages/containers/{id}/seal"].Post)
}

func TestOpenAPIDeclaresPackageBrowseContract(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	for path, operationID := range map[string]string{
		"/api/v1/packages":                                    "listPackages",
		"/api/v1/packages/by-id/{package_id}":                 "getPackage",
		"/api/v1/packages/by-id/{package_id}/members":         "listPackageMembers",
		"/api/v1/packages/label-candidates":                   "listPackageLabelCandidates",
		"/api/v1/packages/field-catalog":                      "listPackageFieldCatalog",
		"/api/v1/packages/by-id/{package_id}/timeline-inputs": "listPackageTimelineInputs",
	} {
		operation := doc.Paths[path].Get
		require.NotNil(t, operation, path)
		assert.Equal(t, operationID, operation.OperationID)
		assert.Contains(t, operation.Responses, "default", path)
	}
	for _, path := range []string{
		"/api/v1/packages", "/api/v1/packages/by-id/{package_id}/members",
		"/api/v1/packages/label-candidates",
		"/api/v1/packages/by-id/{package_id}/timeline-inputs",
	} {
		parameters := map[string]*huma.Param{}
		for _, parameter := range doc.Paths[path].Get.Parameters {
			parameters[parameter.Name] = parameter
		}
		require.Contains(t, parameters, "limit", path)
		assert.Equal(t, new(float64(250)), parameters["limit"].Schema.Maximum, path)
	}
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

func TestProcessingMutationRoutesClearBodyReadDeadline(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	for _, operation := range []*huma.Operation{
		doc.Paths["/api/v1/processing/jobs"].Post,
		doc.Paths["/api/v1/derivatives/purge-jobs"].Post,
	} {
		require.NotNil(t, operation)
		assert.Negative(t, operation.BodyReadTimeout)
	}
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

func TestOpenAPIDeclaresMediaSubmissionBodies(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	schemas := doc.Components.Schemas.Map()
	for _, test := range []struct {
		path, operationID, metadataField string
	}{
		{"/api/v1/media/sources", "submitMediaSource", "occurrence"},
		{"/api/v1/media/sources/{source_id}/artifacts", "importMediaArtifact", "occurrence_id"},
	} {
		t.Run(test.operationID, func(t *testing.T) {
			op := doc.Paths[test.path].Post
			require.NotNil(t, op)
			assert.Equal(t, test.operationID, op.OperationID)
			require.NotNil(t, op.RequestBody)
			assert.True(t, op.RequestBody.Required)
			form := op.RequestBody.Content["multipart/form-data"]
			require.NotNil(t, form)
			assert.ElementsMatch(t, []string{"metadata", "file"}, form.Schema.Required)
			assert.Equal(t, "object", form.Schema.Type)
			file := form.Schema.Properties["file"]
			require.NotNil(t, file)
			assert.Equal(t, "string", file.Type)
			assert.Equal(t, "binary", file.Format)
			require.NotNil(t, form.Encoding["metadata"])
			assert.Equal(t, "application/json", form.Encoding["metadata"].ContentType)
			require.NotNil(t, form.Encoding["file"])
			assert.Equal(t, "*/*", form.Encoding["file"].ContentType)
			metadata := resolveOpenAPISchema(t, schemas, form.Schema.Properties["metadata"])
			for _, field := range []string{"operation_id", "filename", "media_type", "sha256", "byte_length", test.metadataField} {
				assert.Contains(t, metadata.Properties, field)
				assert.Contains(t, metadata.Required, field)
			}
			if test.operationID == "submitMediaSource" {
				body := op.RequestBody.Content["application/json"]
				require.NotNil(t, body)
				reference := resolveOpenAPISchema(t, schemas, body.Schema)
				for _, field := range []string{"operation_id", "reference_url", "occurrence"} {
					assert.Contains(t, reference.Properties, field)
					assert.Contains(t, reference.Required, field)
				}
				canonicalURL := reference.Properties["canonical_url"]
				require.NotNil(t, canonicalURL)
				require.NotNil(t, canonicalURL.MaxLength)
				assert.Equal(t, 8192, *canonicalURL.MaxLength)
				assert.True(t, canonicalURL.WriteOnly)
				assert.NotContains(t, reference.Required, "canonical_url")
			} else {
				require.Len(t, op.Parameters, 1)
				parameter := op.Parameters[0]
				assert.Equal(t, "source_id", parameter.Name)
				assert.Equal(t, "path", parameter.In)
				assert.True(t, parameter.Required)
				assert.Equal(t, "string", parameter.Schema.Type)
			}
		})
	}
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
	assert.NotContains(t, collection.Properties, "coverage")
	quality := doc.Paths["/api/v1/collections/{id}/quality"].Get
	require.NotNil(t, quality)
	qualityCollection := schemas["CollectionQualitySummary"]
	require.NotNil(t, qualityCollection)
	assert.Contains(t, qualityCollection.Required, "coverage")
	coverage := qualityCollection.Properties["coverage"]
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
