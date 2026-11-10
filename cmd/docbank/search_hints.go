package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type searchHint struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lexicalSearchJSON struct {
	api.SearchReport `json:",inline"`

	Hints []searchHint `json:"hints,omitzero"`
}

func emptySearchHints(query string, coverage api.SearchTextCoverage, lexical, scoped, fuzzy bool) []searchHint {
	const fileNoun = "file"
	var hints []searchHint
	if coverage.PDFPageCoverageUnavailable > 0 {
		hints = append(hints, searchHint{Code: "pdf_page_coverage_unavailable", Message: "PDF page coverage is unavailable; some pages may lack searchable text."})
	}
	if coverage.PDFPageCoveragePartial {
		hints = append(hints, searchHint{Code: "pdf_page_coverage_partial", Message: "PDF page coverage is incomplete; narrow the search scope to inspect more PDFs."})
	}
	if coverage.PagesWithoutText > 0 {
		pdf, verb, page := "PDFs", "have", "pages"
		if coverage.PDFsWithoutText == 1 {
			pdf, verb = "PDF", "has"
		}
		if coverage.PagesWithoutText == 1 {
			page = "page"
		}
		lowerBound := ""
		if coverage.PDFPageCoveragePartial {
			lowerBound = "at least "
		}
		hints = append(hints, searchHint{Code: "needs_ocr", Message: fmt.Sprintf("%s%d %s %s %s%d %s with no text layer; if they are scans, OCR can read them. See https://docbank.ai/docs/usage/document-processing/#ocr", lowerBound, coverage.PDFsWithoutText, pdf, verb, lowerBound, coverage.PagesWithoutText, page)})
	}
	if coverage.NoText > 0 {
		labels := map[string]string{"pdf": "PDF", "office": "Office", "other": "other formats"}
		families := make([]string, 0, len(coverage.Families))
		for _, family := range coverage.Families {
			families = append(families, labels[family])
		}
		noun, verb, names := "files", "have", "their names"
		if coverage.NoText == 1 {
			noun, verb, names = fileNoun, "has", "its name"
		}
		hints = append(hints, searchHint{
			Code: "no_searchable_text",
			Message: fmt.Sprintf("%d %s (%s) %s no searchable text, only %s; "+
				"use or set up a profile that can extract these files; build it; choose another profile if a build returns no text; "+
				"list profiles with docbank processing profiles",
				coverage.NoText, noun, strings.Join(families, ", "), verb, names),
		})
	}
	if coverage.Pending > 0 {
		noun, verb := "files", "are"
		if coverage.Pending == 1 {
			noun, verb = fileNoun, "is"
		}
		hints = append(hints, searchHint{Code: "indexing_pending", Message: fmt.Sprintf("%d %s %s still being indexed; search again later", coverage.Pending, noun, verb)})
	}
	terms := store.SearchRequiredTerms(query)
	if lexical && !fuzzy && len(terms) > 1 {
		scope := ""
		if scoped {
			scope = " in this search scope"
		}
		hints = append(hints, searchHint{Code: "every_word_required", Message: fmt.Sprintf("no name or text segment%s matches all required terms (%s); try fewer terms if some match", scope, strings.Join(terms, ", "))})
	}
	return hints
}

func writeSearchHints(cmd *cobra.Command, hints []searchHint) error {
	for _, hint := range hints {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Hint: "+hint.Message); err != nil {
			return fmt.Errorf("writing search hint: %w", err)
		}
	}
	return nil
}
