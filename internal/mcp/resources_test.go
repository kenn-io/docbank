package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestRenditionResourceURIIsCanonicalAndStrict(t *testing.T) {
	identity := renditionResourceIdentity{
		VaultID: "11111111-1111-4111-8111-111111111111", NodeID: 7,
		ContentVersionID: "22222222-2222-4222-8222-222222222222",
		AttachmentID:     strings.Repeat("a", 64),
	}
	canonical := "docbank://vaults/11111111-1111-4111-8111-111111111111/documents/7/" +
		"versions/22222222-2222-4222-8222-222222222222/renditions/" + strings.Repeat("a", 64)
	assert.Equal(t, canonical, renditionResourceURI(identity))

	parsed, window, err := parseRenditionResourceURI(canonical + "?offset=3&max_chars=17")
	require.NoError(t, err)
	assert.Equal(t, identity, parsed)
	assert.Equal(t, renditionWindow{Offset: 3, MaxChars: 17}, window)

	parsed, window, err = parseRenditionResourceURI(canonical)
	require.NoError(t, err)
	assert.Equal(t, identity, parsed)
	assert.Equal(t, renditionWindow{MaxChars: defaultRenditionChars}, window)

	for _, value := range []string{
		canonical + "/extra",
		strings.Replace(canonical, "docbank://vaults/", "docbank://other/", 1),
		strings.Replace(canonical, "/documents/7/", "/documents/07/", 1),
		strings.Replace(canonical, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		canonical + "?offset=-1",
		canonical + "?offset=2147483648",
		canonical + "?offset=999999999999999999999999999999999999",
		canonical + "?offset=1&offset=2",
		canonical + "?max_chars=16001",
		canonical + "?unknown=1",
		canonical + "#fragment",
	} {
		t.Run(value, func(t *testing.T) {
			_, _, err := parseRenditionResourceURI(value)
			require.Error(t, err)
		})
	}
}

func TestRenditionResourceTemplatePinsRFC6570Window(t *testing.T) {
	assert.Equal(t,
		"docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}{?offset,max_chars}",
		renditionResourceTemplate,
	)
}

func TestServerRegistersReadHandlersAndEmptyResourceCatalog(t *testing.T) {
	daemon := newReadToolDaemon(t)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, "synthetic-key"), nil
	}, func(*client.Client) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	toolResult := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_vault_info", "arguments": map[string]any{},
	})))
	assert.NotEqual(t, true, toolResult["isError"])
	structured := objectField(t, toolResult, "structuredContent")
	assert.Equal(t, testVaultID, structured["vault_id"])
	assert.EqualValues(t, 0, structured["ttlMs"])
	assert.Equal(t, "private", structured["cacheScope"])

	resources := decodeResult(t, exchangeRaw(t, server, requestFor("resources/list", map[string]any{})))
	assert.Empty(t, resources["resources"])
	assert.EqualValues(t, resourceCatalogTTLMs, resources["ttlMs"])
	assert.Equal(t, "public", resources["cacheScope"])

	templates := decodeResult(t, exchangeRaw(t, server, requestFor("resources/templates/list", map[string]any{})))
	assert.EqualValues(t, resourceCatalogTTLMs, templates["ttlMs"])
	assert.Equal(t, "public", templates["cacheScope"])
	listed, ok := templates["resourceTemplates"].([]any)
	require.True(t, ok)
	require.Len(t, listed, 1)
	template, ok := listed[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, renditionResourceTemplate, template["uriTemplate"])
	assert.Equal(t, "text/markdown", template["mimeType"])

	read := decodeResult(t, exchangeRaw(t, server, requestFor("resources/read", map[string]any{
		"uri": canonicalTestRenditionURI() + "?offset=1&max_chars=3",
	})))
	assert.EqualValues(t, 0, read["ttlMs"])
	assert.Equal(t, "private", read["cacheScope"])
	contents, ok := read["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 1)
	content, ok := contents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "é界🙂", content["text"])
	assert.Equal(t, "text/markdown", content["mimeType"])
	meta := objectField(t, content, "_meta")
	assert.EqualValues(t, 1, meta["requestedOffset"])
	assert.EqualValues(t, 4, meta["nextOffset"])

	mismatch := strings.Replace(canonicalTestRenditionURI(), "/documents/7/", "/documents/8/", 1)
	wireErr := decodeWireError(t, exchangeRaw(t, server, requestFor("resources/read", map[string]any{
		"uri": mismatch + "?offset=1&max_chars=3",
	})))
	assert.Equal(t, int64(-32603), wireErr.Code)
}

func TestRenditionToolAndResourceRejectOversizedWindowMetadata(t *testing.T) {
	const maxPublishedOffset = 1<<31 - 1
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/renditions/windows", request.URL.Path)
		writeDaemonJSON(t, response, api.RenditionTextWindow{VaultID: testVaultID, NodeID: 7,
			ContentVersionID: testVersionID, AttachmentID: testAttachmentID,
			BuildID: testBuildID, ProfileFingerprint: testProfileID, Text: "x",
			MediaType: "text/markdown", Checksum: strings.Repeat("d", 64),
			RequestedOffset: maxPublishedOffset, ActualStart: maxPublishedOffset,
			ActualEnd: maxPublishedOffset + 1, NextOffset: maxPublishedOffset + 1,
			EOF: true, ResponseBytes: 1,
		})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	toolErr := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "read_rendition_text", "arguments": map[string]any{
			"vault_id": testVaultID, "node_id": 7, "content_version_id": testVersionID,
			"attachment_id": testAttachmentID, "offset": maxPublishedOffset, "max_chars": 1,
		},
	})))
	assert.Equal(t, int64(-32603), toolErr.Code)

	resourceErr := decodeWireError(t, exchangeRaw(t, server, requestFor("resources/read", map[string]any{
		"uri": canonicalTestRenditionURI() + "?offset=2147483647&max_chars=1",
	})))
	assert.Equal(t, int64(-32603), resourceErr.Code)
}

func TestSearchToolWireResultIncludesExactCanonicalRenditionLink(t *testing.T) {
	daemon := newReadToolDaemon(t)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, "synthetic-key"), nil
	}, func(*client.Client) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "search_documents", "arguments": map[string]any{
			"query": "synthetic", "profile": "local", "mode": "lexical", "limit": 1,
			"content_version_ids": []string{testVersionID},
		},
	})))
	content, ok := result["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
	link, ok := content[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "resource_link", link["type"])
	assert.Equal(t, canonicalTestRenditionURI(), link["uri"])
}
