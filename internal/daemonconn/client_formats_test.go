package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	documentcoverage "go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/api"
	internalformatcoverage "go.kenn.io/docbank/internal/formatcoverage"
	"go.kenn.io/docbank/internal/processing"
)

func TestClientFormatCapabilitiesValidatesAndPreservesLookup(t *testing.T) {
	snapshot, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	lookup := documentcoverage.Lookup(snapshot, "wpd")
	snapshot.Formats = []document.FormatCapabilityV1{}
	response := api.FormatCoverageResponse{FormatCoverageV1: snapshot, Lookup: &lookup}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/formats/capabilities", r.URL.Path)
		assert.Equal(t, "extension=wpd&family=archive", r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, response))
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "").FormatCapabilities(t.Context(), "archive", "", "wpd")
	require.NoError(t, err)
	require.NoError(t, document.ValidateFormatCoverageV1(got.FormatCoverageV1))
	require.NotNil(t, got.Lookup)
	assert.Equal(t, document.FormatLookupPending, got.Lookup.Match)
	assert.Equal(t, "wpd", got.Lookup.Query)
	assert.Equal(t, "DB-42b", got.Lookup.Pending.OwnerSlice)
}

func TestClientFormatCapabilitiesAcceptsFamilyExcludedLookup(t *testing.T) {
	snapshot, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	lookup := documentcoverage.Lookup(snapshot, "pdf")
	snapshot.Formats = []document.FormatCapabilityV1{}
	server := serveFormatCapabilities(t, api.FormatCoverageResponse{
		FormatCoverageV1: snapshot, Lookup: &lookup,
	})

	got, err := New(server.URL, "").FormatCapabilities(t.Context(), "archive", "pdf", "")
	require.NoError(t, err)
	require.NotNil(t, got.Lookup)
	assert.Equal(t, "pdf", got.Lookup.Format.ID)
	assert.Empty(t, got.Formats)
}

func TestClientFormatCapabilitiesRejectsInvalidDomainPayload(t *testing.T) {
	snapshot, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	snapshot.Formats[0].Capabilities[document.CapabilityDetect] = document.CapabilityStateV1{
		State: "invented_state",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.FormatCoverageResponse{FormatCoverageV1: snapshot}))
	}))
	t.Cleanup(server.Close)

	_, err = New(server.URL, "").FormatCapabilities(t.Context(), "", "", "")
	require.Error(t, err)
	assert.True(t, IsResponseDecodeError(err))
	assert.ErrorContains(t, err, "unknown state")
}

func TestClientFormatCapabilitiesRejectsMismatchedLookup(t *testing.T) {
	snapshot, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	lookup := documentcoverage.Lookup(snapshot, "wpd")
	lookup.Query = "wrong"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.FormatCoverageResponse{
			FormatCoverageV1: snapshot, Lookup: &lookup,
		}))
	}))
	t.Cleanup(server.Close)

	_, err = New(server.URL, "").FormatCapabilities(t.Context(), "", "", "wpd")
	require.Error(t, err)
	assert.True(t, IsResponseDecodeError(err))
	assert.ErrorContains(t, err, "lookup query")
}

func TestClientFormatCapabilitiesStrictResponseJSON(t *testing.T) {
	base, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	lookup := documentcoverage.Lookup(base, "zip")
	base.Formats = []document.FormatCapabilityV1{*lookup.Format}
	response := api.FormatCoverageResponse{FormatCoverageV1: base, Lookup: &lookup}
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	var ordinary map[string]any
	require.NoError(t, json.Unmarshal(raw, &ordinary))

	t.Run("unknown nested domain member", func(t *testing.T) {
		payload := cloneJSONMap(t, ordinary)
		formats, ok := payload["formats"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, formats)
		row, ok := formats[0].(map[string]any)
		require.True(t, ok)
		row["unexpected"] = true
		server := serveFormatCapabilitiesJSON(t, marshalJSON(t, payload))
		_, err := New(server.URL, "").FormatCapabilities(t.Context(), "", "zip", "")
		require.Error(t, err)
		assert.True(t, IsResponseDecodeError(err))
		assert.ErrorContains(t, err, "unknown object member name")
	})

	t.Run("unknown lookup member", func(t *testing.T) {
		payload := cloneJSONMap(t, ordinary)
		lookup, ok := payload["lookup"].(map[string]any)
		require.True(t, ok)
		lookup["unexpected"] = true
		server := serveFormatCapabilitiesJSON(t, marshalJSON(t, payload))
		_, err := New(server.URL, "").FormatCapabilities(t.Context(), "", "zip", "")
		require.Error(t, err)
		assert.True(t, IsResponseDecodeError(err))
		assert.ErrorContains(t, err, "unknown object member name")
	})

	t.Run("framework schema envelope with ordinary JSON", func(t *testing.T) {
		payload := cloneJSONMap(t, ordinary)
		payload["$schema"] = "http://127.0.0.1/schema/openapi.json"
		encoded := append([]byte(" \n\t"), marshalJSON(t, payload)...)
		server := serveFormatCapabilitiesJSON(t, append(encoded, '\n'))
		got, err := New(server.URL, "").FormatCapabilities(t.Context(), "", "zip", "")
		require.NoError(t, err)
		assert.Equal(t, "zip", got.Lookup.Format.ID)
	})
}

func serveFormatCapabilities(t *testing.T, response api.FormatCoverageResponse) *httptest.Server {
	t.Helper()
	return serveFormatCapabilitiesJSON(t, marshalJSON(t, response))
}

func serveFormatCapabilitiesJSON(t *testing.T, raw []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write(raw)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return server
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	var clone map[string]any
	require.NoError(t, json.Unmarshal(marshalJSON(t, value), &clone))
	return clone
}
