package voyage_test

import (
	"context"
	json "encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/voyage"
)

type embeddingResolver struct{ address netip.Addr }

func (resolver embeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

func voyageFixture(t *testing.T, handler http.Handler) (string, providerhttp.EgressPolicy, providerhttp.Resolver) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	return "http://voyage.invalid:" + portText + "/v1", providerhttp.EgressPolicy{
		Scheme: "http", Host: "voyage.invalid", Port: uint16(port),
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}, embeddingResolver{address: netip.MustParseAddr("127.0.0.1")}
}

func TestContextualEmbeddingUsesDocumentedWireShape(t *testing.T) {
	var request map[string]any
	endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		assert.NoError(t, json.UnmarshalRead(incoming.Body, &request))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(contextualOfficialBody(t, []string{"document envelope: first", "document envelope: second"}, []int{1, 0}))
	}))
	profile := voyageTextProfile(t, voyage.EmbeddingModeContextual)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile = refingerprintVoyageProfile(t, profile)
	provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.NoError(t, err)
	inputs := []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second"},
	}
	result, err := provider.Embed(t.Context(), inputs, voyageAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.NotContains(t, request, "truncation")
	assert.Equal(t, []any{[]any{"document envelope: first", "document envelope: second"}}, request["inputs"])
	assert.InDelta(t, float32(1), result.Vectors[0].Values[1], 0)
	assert.InDelta(t, float32(1), result.Vectors[1].Values[2], 0)
}

func TestContextualEmbeddingStrictlyRejectsDocumentedShapeDrift(t *testing.T) {
	texts := []string{"document envelope: first", "document envelope: second"}
	valid := contextualOfficialBody(t, texts, []int{0, 1})
	tests := []struct {
		name string
		body []byte
	}{
		{"unknown", []byte(strings.Replace(string(valid), `"chunker_version":`, `"unknown":"PRIVATE_RAW_BODY","chunker_version":`, 1))},
		{"duplicate", []byte(strings.Replace(string(valid), `"model":`, `"model":"duplicate","model":`, 1))},
		{"model drift", []byte(strings.Replace(string(valid), voyage.ContextualModel, "voyage-context-drift", 1))},
		{"chunker drift", []byte(strings.Replace(string(valid), `"1.0.0"`, `"2.0.0"`, 1))},
		{"text drift", []byte(strings.Replace(string(valid), texts[0], "PRIVATE_RETURNED_TEXT", 1))},
		{"partial indices", contextualOfficialBody(t, texts[:1], []int{0})},
		{"duplicate index", contextualOfficialBody(t, []string{texts[0], texts[0]}, []int{0, 0})},
	}
	inputs := []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(test.body)
			}))
			profile := voyageTextProfile(t, voyage.EmbeddingModeContextual)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			profile = refingerprintVoyageProfile(t, profile)
			provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "PRIVATE_SECRET"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), inputs, voyageAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			assert.NotContains(t, err.Error(), "PRIVATE")
		})
	}
}

func TestHostedVoyageAliasIsAlwaysExportOnly(t *testing.T) {
	endpoint, egress, resolver := voyageFixture(t, http.NotFoundHandler())
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportsTextQuery = true
	_, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "export-only")
}

func TestVoyageRoleContractMustMatchNativeModes(t *testing.T) {
	endpoint, egress, resolver := voyageFixture(t, http.NotFoundHandler())
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument}
	_, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "native role")
}

func contextualOfficialBody(t *testing.T, texts []string, indices []int) []byte {
	t.Helper()
	items := make([]map[string]any, len(indices))
	for position, index := range indices {
		items[position] = map[string]any{"embedding": unitEmbedding(index + 1), "index": index, "text": texts[index]}
	}
	body, err := json.Marshal(map[string]any{
		"data": []any{map[string]any{"data": items, "index": 0}}, "model": voyage.ContextualModel,
		"usage": map[string]any{"total_tokens": 2}, "chunker_version": "1.0.0",
	})
	require.NoError(t, err)
	return body
}

func refingerprintVoyageProfile(t *testing.T, profile voyage.EmbeddingProfile) voyage.EmbeddingProfile {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, _ = document.NewEmbeddingDescriptor(profile.Descriptor)
	fingerprint, err := voyage.EmbeddingPolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return profile
}
