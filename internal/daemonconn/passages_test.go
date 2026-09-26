package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

func TestPassageOutlineUsesTypedRouteAndPreservesAbsentLocator(t *testing.T) {
	ref, _ := daemonPassageRefs(t)
	want := api.PassageOutline{BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
		Sections: []api.PassageOutlineSection{{Key: strings.Repeat("f", 64), Level: 0, Occurrence: 1,
			ByteStart: 0, ByteEnd: 10, OwnByteEnd: 10, EstimatedUTF8Bytes: 10,
			EstimatedRunes: 7, Preamble: true, Children: []api.PassageOutlineSection{}}}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/api/v1/documents/outline", request.URL.Path)
		assert.Equal(t, "synthetic-key", request.Header.Get("X-Api-Key"))
		var got api.PassageOutlineRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &got)) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, ref, got.Ref)
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, want))
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "synthetic-key").PassageOutline(t.Context(), api.PassageOutlineRequest{Ref: ref})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.Nil(t, got.Sections[0].SourceLocator)
}

func TestReadPassageSectionValidatesExactPageBinding(t *testing.T) {
	ref, body := daemonPassageRefs(t)
	pageRef, err := document.NewPassageRefV1(ref, body, 0, len(body))
	require.NoError(t, err)
	key := strings.Repeat("f", 64)
	request := api.PassageReadSectionRequest{Ref: ref, NavigationKey: key, MaxBytes: 4096}
	want := api.PassageSectionPage{BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
		Section: api.PassageSectionSelection{Key: key, ByteStart: 0, ByteEnd: len(body)},
		Text:    string(body), Ref: &pageRef, PageStart: 0, PageEnd: len(body), Complete: true}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, daemonRequest *http.Request) {
		assert.Equal(t, "/api/v1/passages/read-section", daemonRequest.URL.Path)
		var got api.PassageReadSectionRequest
		if !assert.NoError(t, json.UnmarshalRead(daemonRequest.Body, &got)) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, request, got)
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, want))
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "").ReadPassageSection(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadPassageSectionAcceptsFullRequestedPage(t *testing.T) {
	base, _ := daemonPassageRefs(t)
	body := []byte(strings.Repeat("x", 4096))
	ref, err := document.NewPassageRefV1(base, body, 0, len(body))
	require.NoError(t, err)
	key := strings.Repeat("f", 64)
	request := api.PassageReadSectionRequest{Ref: ref, NavigationKey: key, MaxBytes: len(body)}
	page := api.PassageSectionPage{BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
		Section: api.PassageSectionSelection{Key: key, ByteStart: 0, ByteEnd: len(body)},
		Text:    string(body), Ref: &ref, PageEnd: len(body), Complete: true}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, page))
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "").ReadPassageSection(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, page, got)
}

func TestReadPassageSectionRejectsTextAboveRequestedLimit(t *testing.T) {
	base, _ := daemonPassageRefs(t)
	body := []byte(strings.Repeat("x", 4097))
	ref, err := document.NewPassageRefV1(base, body, 0, len(body))
	require.NoError(t, err)
	key := strings.Repeat("f", 64)
	page := api.PassageSectionPage{BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
		Section: api.PassageSectionSelection{Key: key, ByteStart: 0, ByteEnd: len(body)},
		Text:    string(body), Ref: &ref, PageEnd: len(body), Complete: true}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, page))
	}))
	t.Cleanup(server.Close)

	_, err = New(server.URL, "").ReadPassageSection(t.Context(), api.PassageReadSectionRequest{
		Ref: ref, NavigationKey: key, MaxBytes: 4096,
	})
	require.ErrorContains(t, err, "text exceeds requested limit")
}

func TestPassageClientRejectsMismatchedAndOversizedResponses(t *testing.T) {
	ref, body := daemonPassageRefs(t)
	pageRef, err := document.NewPassageRefV1(ref, body, 0, len(body))
	require.NoError(t, err)
	key := strings.Repeat("f", 64)
	request := api.PassageReadSectionRequest{Ref: ref, NavigationKey: key, MaxBytes: 4096}
	valid := api.PassageSectionPage{BodySHA256: ref.BodySHA256, RenditionBuildID: ref.RenditionBuildID,
		Section: api.PassageSectionSelection{Key: key, ByteStart: 0, ByteEnd: len(body)},
		Text:    string(body), Ref: &pageRef, PageEnd: len(body), Complete: true}
	for name, mutate := range map[string]func(*api.PassageSectionPage){
		"body":         func(page *api.PassageSectionPage) { page.BodySHA256 = strings.Repeat("0", 64) },
		"section":      func(page *api.PassageSectionPage) { page.Section.Key = strings.Repeat("1", 64) },
		"range":        func(page *api.PassageSectionPage) { page.PageEnd-- },
		"quote":        func(page *api.PassageSectionPage) { page.Ref.QuoteSHA256 = strings.Repeat("2", 64) },
		"continuation": func(page *api.PassageSectionPage) { page.Continuation = "unexpected" },
	} {
		t.Run(name, func(t *testing.T) {
			page := valid
			copiedRef := *valid.Ref
			page.Ref = &copiedRef
			mutate(&page)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(response, page))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "").ReadPassageSection(t.Context(), request)
			require.Error(t, err)
		})
	}

	t.Run("oversized outline", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, writeErr := response.Write([]byte(strings.Repeat("x", maxPassageOutlineResponseBytes+1)))
			assert.NoError(t, writeErr)
		}))
		t.Cleanup(server.Close)
		_, err := New(server.URL, "").PassageOutline(t.Context(), api.PassageOutlineRequest{Ref: ref})
		require.Error(t, err)
	})
}

func daemonPassageRefs(t *testing.T) (document.PassageRefV1, []byte) {
	t.Helper()
	body := []byte("😀 exact")
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64),
	}, body, 0, len(body))
	require.NoError(t, err)
	return ref, body
}
