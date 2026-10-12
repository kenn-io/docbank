package daemonconn

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestTextNavigationClient(t *testing.T) {
	t.Parallel()
	citation := clientCitation(t).Citation
	request := document.TextOutlineRequest{Citation: citation}
	valid := document.TextOutlinePage{Citation: citation, DocumentChars: 9,
		EnclosingSection: document.TextSectionRange{Start: 0, End: 9}, Total: 2,
		Sections: []document.TextOutlineEntry{{Start: 0, End: 9}, {Start: 0, End: 9, Level: 1, TitleMarkdown: "T"}}}
	for name, mutate := range map[string]func(*document.TextOutlinePage){
		"valid duplicate bounds": nil,
		"identity":               func(p *document.TextOutlinePage) { p.Citation.NodeID++ },
		"length":                 func(p *document.TextOutlinePage) { p.DocumentChars = 8 },
		"enclosure":              func(p *document.TextOutlinePage) { p.EnclosingSection.Start = 2 },
		"root":                   func(p *document.TextOutlinePage) { p.Sections[0].Level = 1 },
		"root title":             func(p *document.TextOutlinePage) { p.Sections[0].TitleMarkdown = "x" },
		"order":                  func(p *document.TextOutlinePage) { p.Sections = append(p.Sections, p.Sections[1]); p.Total++ },
		"label":                  func(p *document.TextOutlinePage) { p.Sections[1].TitleMarkdown = strings.Repeat("界", 161) },
		"count":                  func(p *document.TextOutlinePage) { p.Total++ },
		"next":                   func(p *document.TextOutlinePage) { p.NextOffset = new(2) },
		"offset":                 func(p *document.TextOutlinePage) { p.Offset = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			response.Sections = slices.Clone(valid.Sections)
			if mutate != nil {
				mutate(&response)
			}
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/text-citations/outline", r.URL.Path)
				var got document.TextOutlineRequest
				require.NoError(t, json.UnmarshalRead(r.Body, &got))
				require.Equal(t, request, got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			got, err := New(server.URL, "synthetic-key").GetTextOutline(t.Context(), request)
			if mutate == nil {
				require.NoError(t, err)
				require.Equal(t, valid, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestTextNavigationClientSection(t *testing.T) {
	t.Parallel()
	quote := clientCitation(t)
	request := document.TextSectionRequest{Citation: quote.Citation,
		Section: document.TextSectionRange{Start: 0, End: 5}, Offset: 1, MaxChars: 3}
	valid := document.TextSectionPage{Citation: quote.Citation, Section: request.Section, Offset: 1,
		ActualStart: 1, ActualEnd: 4, NextOffset: 4, Text: quote.Text, TextBytes: quote.TextBytes,
		TextSHA256: quote.TextSHA256}
	for name, mutate := range map[string]func(*document.TextSectionPage){
		"valid":    nil,
		"identity": func(p *document.TextSectionPage) { p.Citation.NodeID++ },
		"section":  func(p *document.TextSectionPage) { p.Section.End++ },
		"offset":   func(p *document.TextSectionPage) { p.Offset++ },
		"start":    func(p *document.TextSectionPage) { p.ActualStart++ },
		"end":      func(p *document.TextSectionPage) { p.ActualEnd++ },
		"next":     func(p *document.TextSectionPage) { p.NextOffset++ },
		"complete": func(p *document.TextSectionPage) { p.Complete = true },
		"text length": func(p *document.TextSectionPage) {
			p.Text = "abcdefghi"
			p.TextBytes = 9
			p.TextSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(p.Text)))
		},
		"bytes":  func(p *document.TextSectionPage) { p.TextBytes++ },
		"digest": func(p *document.TextSectionPage) { p.TextSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			if mutate != nil {
				mutate(&response)
			}
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/text-citations/section", r.URL.Path)
				var got document.TextSectionRequest
				require.NoError(t, json.UnmarshalRead(r.Body, &got))
				require.Equal(t, request, got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			got, err := New(server.URL, "synthetic-key").ReadTextSection(t.Context(), request)
			if mutate == nil {
				require.NoError(t, err)
				require.Equal(t, valid, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestTextNavigationClientTransport(t *testing.T) {
	t.Parallel()
	request := document.TextOutlineRequest{Citation: clientCitation(t).Citation, Offset: 1}
	valid := document.TextOutlinePage{Citation: request.Citation, DocumentChars: 5, Total: 1, Offset: 1,
		EnclosingSection: document.TextSectionRange{Start: 0, End: 5}, Sections: []document.TextOutlineEntry{}}
	encoded, err := json.Marshal(valid)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, body string
		ok         bool
		length     string
	}{
		{"terminal", string(encoded), true, ""},
		{"schema", strings.TrimSuffix(string(encoded), "}") + `,"$schema":"synthetic"}`, true, ""},
		{"extra", strings.TrimSuffix(string(encoded), "}") + `,"other":true}`, false, ""},
		{"exact cap", strings.Repeat(" ", (256<<10)-len(encoded)) + string(encoded), true, ""},
		{"over cap", strings.Repeat(" ", (256<<10)-len(encoded)+1) + string(encoded), false, ""},
		{"dropped", `{"citation":`, false, "400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := New(server.URL, "synthetic-key").GetTextOutline(t.Context(), request)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if tc.name == "dropped" {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			}
		})
	}
}
