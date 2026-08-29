package client

import (
	"encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestRenditionTextWindowUsesTypedDaemonRouteAndValidatesAuthority(t *testing.T) {
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	attachmentID := strings.Repeat("a", 64)
	request := api.RenditionWindowRequest{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID, Offset: 3, MaxChars: 3}
	want := api.RenditionTextWindow{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID,
		BuildID: strings.Repeat("b", 64), ProfileFingerprint: strings.Repeat("c", 64),
		Text: "é界🙂", MediaType: "text/markdown", Checksum: strings.Repeat("d", 64),
		RequestedOffset: 3, ActualStart: 3, ActualEnd: 6, NextOffset: 6,
		EOF: false, ResponseBytes: len("é界🙂"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, daemonRequest *http.Request) {
		assert.Equal(t, http.MethodPost, daemonRequest.Method)
		assert.Equal(t, "/api/v1/renditions/windows", daemonRequest.URL.Path)
		assert.Equal(t, "synthetic-key", daemonRequest.Header.Get("X-Api-Key"))
		var got api.RenditionWindowRequest
		if !assert.NoError(t, json.UnmarshalRead(daemonRequest.Body, &got)) {
			return
		}
		assert.Equal(t, request, got)
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, want))
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "synthetic-key").RenditionTextWindow(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRenditionTextWindowValidatesPublishedOffsetArithmetic(t *testing.T) {
	const maxPublishedOffset = 1<<31 - 1
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	attachmentID := strings.Repeat("a", 64)
	request := api.RenditionWindowRequest{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID,
		Offset: maxPublishedOffset, MaxChars: 1}
	valid := api.RenditionTextWindow{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID,
		BuildID: strings.Repeat("b", 64), ProfileFingerprint: strings.Repeat("c", 64),
		MediaType: "text/markdown", Checksum: strings.Repeat("d", 64),
		RequestedOffset: maxPublishedOffset, ActualStart: maxPublishedOffset,
		ActualEnd: maxPublishedOffset, NextOffset: maxPublishedOffset, EOF: true,
	}
	require.NoError(t, validateRenditionTextWindow(request, valid),
		"the published maximum remains a valid empty EOF position")

	for name, mutate := range map[string]func(*api.RenditionTextWindow){
		"semantic overflow": func(window *api.RenditionTextWindow) {
			window.Text = "x"
			window.ActualEnd = maxPublishedOffset + 1
			window.NextOffset = maxPublishedOffset + 1
			window.ResponseBytes = 1
		},
		"max plus one": func(window *api.RenditionTextWindow) {
			window.ActualStart = maxPublishedOffset + 1
		},
		"integer extreme": func(window *api.RenditionTextWindow) {
			window.ActualEnd = math.MaxInt
			window.NextOffset = math.MaxInt
		},
		"interval mismatch": func(window *api.RenditionTextWindow) {
			window.ActualStart--
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			mutate(&response)
			require.Error(t, validateRenditionTextWindow(request, response))
		})
	}
}

func TestRenditionTextWindowRejectsMismatchedOrOversizedResponses(t *testing.T) {
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	attachmentID := strings.Repeat("a", 64)
	request := api.RenditionWindowRequest{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID, MaxChars: 1}
	valid := api.RenditionTextWindow{VaultID: vaultID, NodeID: 7,
		ContentVersionID: versionID, AttachmentID: attachmentID,
		BuildID: strings.Repeat("b", 64), ProfileFingerprint: strings.Repeat("c", 64),
		Text: "x", MediaType: "text/markdown", Checksum: strings.Repeat("d", 64),
		ActualEnd: 1, NextOffset: 1, EOF: true, ResponseBytes: 1,
	}
	for name, mutate := range map[string]func(*api.RenditionTextWindow){
		"node": func(window *api.RenditionTextWindow) { window.NodeID++ },
		"version": func(window *api.RenditionTextWindow) {
			window.ContentVersionID = "33333333-3333-4333-8333-333333333333"
		},
		"attachment": func(window *api.RenditionTextWindow) { window.AttachmentID = strings.Repeat("e", 64) },
		"media":      func(window *api.RenditionTextWindow) { window.MediaType = "application/octet-stream" },
		"text bound": func(window *api.RenditionTextWindow) {
			window.Text, window.ActualEnd, window.NextOffset, window.ResponseBytes = "xx", 2, 2, 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			responseBody := valid
			mutate(&responseBody)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(response, responseBody))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "").RenditionTextWindow(t.Context(), request)
			require.Error(t, err)
		})
	}

	t.Run("oversized body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, err := response.Write([]byte(strings.Repeat("x", maxRenditionWindowResponseBytes+1)))
			assert.NoError(t, err)
		}))
		t.Cleanup(server.Close)
		_, err := New(server.URL, "").RenditionTextWindow(t.Context(), request)
		require.Error(t, err)
	})
}
