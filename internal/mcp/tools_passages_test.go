package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func TestPassageToolsReturnExactBoundedPrivateResults(t *testing.T) {
	body := []byte("# Heading\nsynthetic evidence\n")
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID: "11111111-1111-4111-8111-111111111111", DocumentUID: "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333", SourceSHA256: strings.Repeat("a", 64),
		RenditionBuildID: strings.Repeat("b", 64), AttachmentID: strings.Repeat("c", 64),
	}, body, 0, len(body))
	require.NoError(t, err)
	sectionKey := strings.Repeat("d", 64)
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
		VaultUID: ref.VaultUID, ContentVersionIDs: []string{ref.ContentVersionID}})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/documents/outline":
			var input api.PassageOutlineRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, ref, input.Ref)
			assert.NoError(t, json.MarshalWrite(response, api.PassageOutline{
				BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
				Sections: []api.PassageOutlineSection{{Key: strings.Repeat("e", 64), Level: 0,
					Occurrence: 1, ByteStart: 0, ByteEnd: 0, OwnByteEnd: 0, Preamble: true,
					Children: []api.PassageOutlineSection{}}, {Key: sectionKey, Title: "Heading", Level: 1,
					Occurrence: 1, ByteStart: 0, ByteEnd: len(body), OwnByteEnd: len(body),
					EstimatedUTF8Bytes: len(body), EstimatedRunes: len(body), Children: []api.PassageOutlineSection{}}},
			}))
		case "/api/v1/passages/read-section":
			var input api.PassageReadSectionRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, sectionKey, input.NavigationKey)
			assert.True(t, input.IncludeChildren)
			pageRef, pageErr := document.NewPassageRefV1(ref, body, 0, len(body))
			if !assert.NoError(t, pageErr) {
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			assert.NoError(t, json.MarshalWrite(response, api.PassageSectionPage{
				BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
				Section: api.PassageSectionSelection{Key: sectionKey, Title: "Heading", Level: 1,
					ByteStart: 0, ByteEnd: len(body), IncludeChildren: true},
				Text: string(body), Ref: &pageRef, PageStart: 0, PageEnd: len(body), Complete: true,
			}))
		case "/api/v1/context-packs":
			var input api.ContextPackRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, []string{ref.ContentVersionID}, input.Fence.ContentVersionIDs)
			assert.NoError(t, json.MarshalWrite(response, api.ContextPackResponse(processing.ContextPack{
				FenceFingerprint: fingerprint,
				Coverage:         processing.ContextCoverage{RequestedSources: 1, AvailableSources: 1, SelectedSources: 1},
				Passages: []processing.ContextPassage{{Ref: ref, Text: string(body), Path: "/synthetic.md",
					Reasons: []string{"exact_seed"}}}, Omitted: map[string]int{}, Complete: true,
			})))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })

	outlineResult, err := invokeReadTool(t.Context(), lease, "get_document_outline", map[string]any{"ref": ref})
	require.NoError(t, err)
	outline := structuredMap(t, outlineResult.StructuredContent)
	assert.Equal(t, "private", outline["cacheScope"])
	assert.Equal(t, ref.BodySHA256, outline["body_sha256"])
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false))["get_document_outline"].OutputSchema, outline)

	sectionResult, err := invokeReadTool(t.Context(), lease, "read_passage_section", map[string]any{
		"ref": ref, "navigation_key": sectionKey, "include_children": true, "max_bytes": 4096,
	})
	require.NoError(t, err)
	section := structuredMap(t, sectionResult.StructuredContent)
	assert.Equal(t, "private", section["cacheScope"])
	assert.Equal(t, string(body), section["text"])
	assert.Equal(t, true, section["complete"])
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false))["read_passage_section"].OutputSchema, section)

	contextResult, err := invokeReadTool(t.Context(), lease, "get_context_pack", map[string]any{
		"vault_uid": ref.VaultUID, "content_version_ids": []string{ref.ContentVersionID},
		"seed": ref, "max_bytes": 4096,
	})
	require.NoError(t, err)
	contextPack := structuredMap(t, contextResult.StructuredContent)
	assert.Equal(t, "private", contextPack["cacheScope"])
	assert.Equal(t, fingerprint, contextPack["fence_fingerprint"])
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false))["get_context_pack"].OutputSchema, contextPack)
}

func TestResolvePassageToolReadsSuppliedExactReference(t *testing.T) {
	body := []byte("# Heading\nsynthetic evidence\n")
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID: "11111111-1111-4111-8111-111111111111", DocumentUID: "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333", SourceSHA256: strings.Repeat("a", 64),
		RenditionBuildID: strings.Repeat("b", 64), AttachmentID: strings.Repeat("c", 64),
	}, body, 10, len(body))
	require.NoError(t, err)
	id, err := document.PassageIdentityV1(ref)
	require.NoError(t, err)
	tool := catalogMap(toolCatalog(false, false))["resolve_passage"]
	require.NotNil(t, tool, "agents need a read-only exact passage resolver")
	require.True(t, tool.Annotations.ReadOnlyHint)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/passages/resolve", request.URL.Path)
		var input api.PassageResolveRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, ref, input.Ref)
		assert.Equal(t, 4096, input.MaxBytes)
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, api.PassageResolution{
			Availability: "available", Freshness: "historical", PassageID: id, Ref: ref,
			Text: string(body[10:]), SectionPath: []string{"Heading"}, SourcePath: "/synthetic.md",
			SourceLocator: &document.EvidenceLocatorV1{Kind: "line", IndexOrigin: "one", Start: 2, End: 2},
		}))
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })

	result, err := invokeReadTool(t.Context(), lease, "resolve_passage", map[string]any{"ref": ref, "max_bytes": 4096})
	require.NoError(t, err)
	output := structuredMap(t, result.StructuredContent)
	assert.Equal(t, "private", output["cacheScope"])
	assert.Equal(t, "historical", output["freshness"])
	assert.Equal(t, id, output["passage_id"])
	assert.Equal(t, string(body[10:]), output["text"])
	assert.Equal(t, "/synthetic.md", output["source_path"])
	assert.Equal(t, []any{"Heading"}, output["section_path"])
	assertSchemaAccepts(t, tool.OutputSchema, output)
}
