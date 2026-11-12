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
