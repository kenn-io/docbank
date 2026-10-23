package docling

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestRenderPageRangeKeepsOriginalPageNumbers(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		pages           PageRange
		blank           bool
		partial         bool
		markdown        string
		blankProvenance bool
	}{
		{name: "first range", pages: PageRange{First: 1, Last: 2}},
		{name: "whitespace Markdown blank range", pages: PageRange{First: 1, Last: 2}, blank: true, markdown: " \t\n"},
		{name: "later range", pages: PageRange{First: 51, Last: 52}, partial: true},
		{name: "blank later range", pages: PageRange{First: 51, Last: 52}, blank: true},
		{name: "blank text provenance", pages: PageRange{First: 51, Last: 52}, blankProvenance: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, "pdf", "application/pdf", "range.pdf", []byte("synthetic PDF bytes"))
			var registry []int64
			texts := []any{}
			for page := testCase.pages.First; page <= testCase.pages.Last; page++ {
				registry = append(registry, page)
				if !testCase.blank {
					texts = append(texts, pageText("page "+strconv.FormatInt(page, 10), page))
				}
			}
			if testCase.blankProvenance {
				texts = append(texts, pageText(" \t\n", 53))
			}
			markdown := "# range\n"
			if testCase.blank {
				markdown = testCase.markdown
			}
			status := "success"
			response := rangeResponse(markdown, rangeContent(registry, texts...))
			if testCase.partial {
				status = "partial_success"
				response["errors"] = []any{map[string]any{"page_no": 52}}
			}
			result, err := renderRange(t, fixture, testCase.pages, status, response)
			require.NoError(t, err)
			assert.Equal(t, document.EvidencePartial, result.Evidence.Completeness)
			assertRangeMarker(t, result.Evidence, testCase.pages)
			var preceding []document.SourceEvidenceOmissionV1
			for _, omission := range result.Evidence.Omissions {
				if omission.Reason == "pages precede the requested range" {
					preceding = append(preceding, omission)
				}
			}
			if testCase.pages.First == 1 {
				assert.Empty(t, preceding)
			} else {
				assert.Equal(t, []document.SourceEvidenceOmissionV1{{
					Kind: document.EvidenceOmissionUnit, Reason: "pages precede the requested range",
					Locator: &document.SourceEvidenceLocatorV1{
						Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 50,
					},
				}}, preceding)
			}
			require.Len(t, result.Evidence.Units, len(registry))
			for index, unit := range result.Evidence.Units {
				assert.Equal(t, registry[index], unit.Locator.Start)
				assert.Equal(t, registry[index], unit.Locator.End)
				if testCase.blank {
					assert.Empty(t, unit.Text)
				} else {
					assert.Equal(t, "page "+strconv.FormatInt(registry[index], 10), unit.Text)
				}
			}
			if testCase.partial {
				assert.Empty(t, result.Evidence.Units[0].Omissions)
				require.Len(t, result.Evidence.Units[1].Omissions, 1)
				assert.Equal(t, "provider_output", result.Evidence.Units[1].Omissions[0].Field)
			}
		})
	}
}

func TestRenderPageRangeRejectsContradictorySelection(t *testing.T) {
	selected := PageRange{First: 51, Last: 52}
	ignoredRange := make([]int64, 0, 52)
	for page := int64(1); page <= 52; page++ {
		ignoredRange = append(ignoredRange, page)
	}
	for _, testCase := range []struct {
		name    string
		pages   PageRange
		content map[string]any
	}{
		{name: "server ignored range", pages: selected, content: rangeContent(ignoredRange, pageText("page 51", 51))},
		{name: "wrong registry keys", pages: selected, content: rangeContent([]int64{52, 53}, pageText("page 51", 51))},
		{name: "empty registry", pages: selected, content: rangeContent([]int64{}, pageText("page 51", 51))},
		{name: "provenance outside registry", pages: selected, content: rangeContent([]int64{51, 52}, pageText("page 50", 50))},
		{name: "missing schema", pages: selected},
		{name: "unsupported major", pages: selected, content: func() map[string]any {
			content := rangeContent([]int64{51, 52}, pageText("page 51", 51))
			content["version"] = "2.0.0"
			return content
		}()},
		{name: "provenance with unknown field", pages: PageRange{First: 2, Last: 3}, content: func() map[string]any {
			content := rangeContent([]int64{2, 3}, pageText("outside selection", 1))
			content["future_content"] = []any{}
			return content
		}()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, "pdf", "application/pdf", "range.pdf", []byte("synthetic PDF bytes"))
			_, err := renderRange(t, fixture, testCase.pages, "success", rangeResponse("# fallback\n", testCase.content))
			requireMalformed(t, err)
		})
	}
}

func TestRenderPageRangeFallsBackToMarkdownLikeRender(t *testing.T) {
	pages := PageRange{First: 51, Last: 52}
	content := rangeContent([]int64{51, 52})
	content["tables"] = []any{map[string]any{"data": map[string]any{"table_cells": []any{map[string]any{"text": "table cell"}}}}}
	fixture := newFixture(t, "pdf", "application/pdf", "range.pdf", []byte("synthetic PDF bytes"))
	result, err := renderRange(t, fixture, pages, "success", rangeResponse("# fallback\n", content))
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, "# fallback\n", result.Evidence.Units[0].Text)
	assertRangeMarker(t, result.Evidence, pages)
}

func TestRenderPageRangeRejectsInvalidRequestsBeforeUpload(t *testing.T) {
	pdf := newFixture(t, "pdf", "application/pdf", "range.pdf", []byte("synthetic PDF bytes"))
	text := newFixture(t, "text", "text/plain", "range.txt", []byte("synthetic text bytes"))
	for _, testCase := range []struct {
		name    string
		fixture fixture
		pages   PageRange
	}{
		{name: "zero first", fixture: pdf, pages: PageRange{First: 0, Last: 2}},
		{name: "last before first", fixture: pdf, pages: PageRange{First: 3, Last: 2}},
		{name: "non PDF", fixture: text, pages: PageRange{First: 1, Last: 2}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var requests, reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				requests.Add(1)
			}))
			t.Cleanup(server.Close)
			upload := &testUpload{Reader: readerFunc(func([]byte) (int, error) {
				reads.Add(1)
				return 0, errors.New("upload must not be read")
			}), metadata: testCase.fixture.metadata}

			client := newClient(t, server.URL, testCase.fixture.descriptor, nil, http.DefaultClient)
			_, err := client.RenderPageRange(t.Context(), upload, testCase.fixture.authorization, testCase.pages)
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, document.RenditionErrorPolicyRejected, providerErr.Code())
			assert.Zero(t, requests.Load())
			assert.Zero(t, reads.Load())
		})
	}
}

// renderRange serves one Docling job and checks its submitted page range.
func renderRange(
	t *testing.T, fixture fixture, pages PageRange, taskStatus string, response map[string]any,
) (document.RenditionResult, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			assertDoclingSubmission(t, request, fixture.metadata, fixture.source, []string{"md", "json"},
				[]string{strconv.FormatInt(pages.First, 10), strconv.FormatInt(pages.Last, 10)})
			writeJSON(t, writer, doclingTask("range", taskStatus))
		case resultPath + "range":
			writeJSON(t, writer, response)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	return client.RenderPageRange(t.Context(), fixture.upload(), fixture.authorization, pages)
}

func rangeResponse(markdown string, content map[string]any, options ...resultOption) map[string]any {
	response := doclingResultResponse("range.pdf", markdown, nil, options...)
	doc, ok := response["document"].(map[string]any)
	if !ok {
		panic("Docling result fixture document must be an object")
	}
	doc["json_content"] = content
	return response
}

func rangeContent(registry []int64, texts ...any) map[string]any {
	var pages map[string]any
	if registry != nil {
		pages = make(map[string]any, len(registry))
		for _, page := range registry {
			pages[strconv.FormatInt(page, 10)] = map[string]any{}
		}
	}
	return doclingDocumentContent("range.pdf", append([]any{}, texts...), pages)
}

func pageText(text string, page int64) map[string]any {
	return map[string]any{"text": text, "prov": []any{map[string]any{"page_no": page}}}
}

func assertRangeMarker(t *testing.T, evidence document.SourceEvidenceV1, pages PageRange) {
	t.Helper()
	assert.Contains(t, evidence.Omissions, document.SourceEvidenceOmissionV1{
		Kind: document.EvidenceOmissionField, Field: PageRangeOmissionField,
		Reason: "selected pages " + strconv.FormatInt(pages.First, 10) + ".." + strconv.FormatInt(pages.Last, 10),
	})
}

func requireMalformed(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, err.Error())
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
	assert.False(t, document.IsRenditionProviderErrorRetryable(err))
}
