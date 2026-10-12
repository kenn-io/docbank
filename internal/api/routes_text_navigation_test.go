package api_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"go.kenn.io/docbank/internal/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

const outlinePath = "/api/v1/text-citations/outline"
const sectionPath = "/api/v1/text-citations/section"

func TestTextNavigationHTTP(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	request := document.TextOutlineRequest{Citation: f.citation}
	resp, body := do(t, f.server, http.MethodPost, outlinePath, nil, request)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var outline document.TextOutlinePage
	require.NoError(t, json.Unmarshal([]byte(body), &outline))
	require.Equal(t, f.citation, outline.Citation)
	require.Equal(t, []document.TextOutlineEntry{{Start: 0, End: 15}}, outline.Sections)
	require.Equal(t, document.TextSectionRange{Start: 0, End: 15}, outline.EnclosingSection)
	section := document.TextSectionRequest{Citation: f.citation, Section: outline.EnclosingSection, MaxChars: 16000}
	resp, body = do(t, f.server, http.MethodPost, sectionPath, nil, section)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var page document.TextSectionPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, "Verified alpha\n", page.Text)
	require.True(t, page.Complete)
	client := daemonconn.New(f.server.URL, testAPIKey)
	got, err := client.GetTextOutline(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, outline, got)
	read, err := client.ReadTextSection(t.Context(), section)
	require.NoError(t, err)
	require.Equal(t, page, read)
	section.Offset = 15
	read, err = client.ReadTextSection(t.Context(), section)
	require.NoError(t, err)
	require.Empty(t, read.Text)
	require.Zero(t, read.TextBytes)
	require.True(t, read.Complete)
	request.Offset = 1
	got, err = client.GetTextOutline(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, got.Sections)
	require.Empty(t, got.Sections)
	request.Offset = 2
	_, err = client.GetTextOutline(t.Context(), request)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "invalid_text_navigation", code)
	for _, path := range []string{outlinePath, sectionPath} {
		var input any = section
		if path == outlinePath {
			input = document.TextOutlineRequest{Citation: f.citation}
		}
		resp, body = do(t, f.server, http.MethodPost, path, map[string]string{
			"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, f.server)}, input)
		require.Equal(t, 403, resp.StatusCode, body)
		require.Contains(t, body, "web_session_read_only")
	}
}

func TestTextNavigationHTTPShape(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	citation, err := json.Marshal(f.citation)
	require.NoError(t, err)
	valid := map[string]any{"citation": jsontext.Value(citation), "offset": 0}
	for name, change := range map[string]func(map[string]any){
		"missing offset":  func(m map[string]any) { delete(m, "offset") },
		"null offset":     func(m map[string]any) { m["offset"] = nil },
		"null citation":   func(m map[string]any) { m["citation"] = nil },
		"extra":           func(m map[string]any) { m["extra"] = true },
		"negative offset": func(m map[string]any) { m["offset"] = -1 },
		"UUID syntax": func(m map[string]any) {
			m["citation"] = jsontext.Value(strings.Replace(
				string(citation), f.citation.VaultUID.String(), "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"citation": valid["citation"], "offset": 0}
			change(input)
			resp, body := do(t, f.server, http.MethodPost, outlinePath, nil, input)
			require.Equal(t, 422, resp.StatusCode, body)
		})
	}
	for _, path := range []string{outlinePath, sectionPath} {
		anchor := f.citation
		anchor.End = anchor.Start + 100
		var input any = document.TextOutlineRequest{Citation: anchor}
		if path == sectionPath {
			input = document.TextSectionRequest{Citation: anchor,
				Section: document.TextSectionRange{Start: 0, End: 15}, MaxChars: 10}
		}
		resp, body := do(t, f.server, http.MethodPost, path, nil, input)
		require.Equal(t, 416, resp.StatusCode, body)
		require.Contains(t, body, "invalid_citation_range")
	}
}

func TestTextNavigationHTTPAvailability(t *testing.T) {
	t.Parallel()
	t.Run("service absent", func(t *testing.T) {
		f := newCitationHTTPFixture(t, api.NewOperationGate())
		server, _ := newTestServer(t, func(d *api.Deps) { d.Processing = nil })
		resp, body := do(t, server, http.MethodPost, outlinePath, nil,
			document.TextOutlineRequest{Citation: f.citation})
		require.Equal(t, 503, resp.StatusCode, body)
		require.Contains(t, body, "processing_unavailable")
	})
	t.Run("heading limit", func(t *testing.T) {
		var cfg config.Config
		server, store := newTestServer(t, func(d *api.Deps) {
			renditionTextConfig(d)
			profile := d.Cfg.ProcessingProfiles["archive"]
			profile.MaxUnitRunes = 100_000
			d.Cfg.ProcessingProfiles["archive"] = profile
			cfg = d.Cfg
			d.Gate = api.NewOperationGate()
			var err error
			d.Processing, err = processing.NewService(processing.ServiceConfig{
				Catalog: d.Store, Blobs: d.Blobs, Gate: d.Gate,
				SpoolDirectory: filepath.Join(d.VaultRoot, "blobs", "tmp"),
			})
			require.NoError(t, err)
		})
		node := createFileWithContent(t, server, store, "/outline.pdf", "%PDF-1.4 synthetic")
		fixture := publishRenditionTextSourceFixture(t, store, cfg, node, strings.Repeat("# h\n\n", 4097))
		vault, err := uuid.Parse(store.VaultID())
		require.NoError(t, err)
		version, err := uuid.Parse(node.CurrentVersionID)
		require.NoError(t, err)
		citation := document.TextCitation{Version: 1, VaultUID: vault, NodeID: node.ID,
			ContentVersionID: version, ContentSHA256: node.BlobHash, BuildID: fixture.build,
			RenditionAttachmentID: fixture.attachment, RenditionSHA256: testHash(string(fixture.markdown)), End: 1}
		resp, body := do(t, server, http.MethodPost, outlinePath, nil, document.TextOutlineRequest{Citation: citation})
		require.Equal(t, 422, resp.StatusCode, body)
		require.Contains(t, body, "text_navigation_limit")
		require.NotContains(t, body, `"sections":`)
	})
}

func TestTextNavigationHTTPSectionShape(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	for name, change := range map[string]func(map[string]any){
		"missing offset": func(m map[string]any) { delete(m, "offset") },
		"missing max":    func(m map[string]any) { delete(m, "max_chars") },
		"null section":   func(m map[string]any) { m["section"] = nil },
		"null max":       func(m map[string]any) { m["max_chars"] = nil },
		"nested extra":   func(m map[string]any) { m["section"] = map[string]any{"start": 0, "end": 15, "extra": true} },
		"not a section":  func(m map[string]any) { m["section"] = map[string]any{"start": 1, "end": 14} },
		"past end":       func(m map[string]any) { m["offset"] = 16 },
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"citation": f.citation, "section": map[string]any{"start": 0, "end": 15},
				"offset": 0, "max_chars": 10}
			change(input)
			resp, body := do(t, f.server, http.MethodPost, sectionPath, nil, input)
			require.Equal(t, 422, resp.StatusCode, body)
			if name == "not a section" || name == "past end" {
				require.Contains(t, body, "invalid_text_navigation")
			}
		})
	}
}
