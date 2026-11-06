package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/docbank/internal/api"
)

type searchHint struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next,omitzero"`
}

type lexicalSearchJSON struct {
	api.SearchReport `json:",inline"`

	Hints []searchHint `json:"hints,omitzero"`
}

func emptySearchHints(cmd *cobra.Command, query string, coverage api.SearchTextCoverage, lexical, scoped, fuzzy bool) []searchHint {
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
				"set up a text-extraction profile; build it; "+
				"list profiles with docbank processing profiles",
				coverage.NoText, noun, strings.Join(families, ", "), verb, names),
		})
	}
	if coverage.Pending > 0 {
		noun, verb := "files", "are"
		if coverage.Pending == 1 {
			noun, verb = fileNoun, "is"
		}
		// Single quotes keep each word literal in bash, zsh, fish, and PowerShell
		// when it has no quote or backslash.
		var words []string
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			if flag.Changed {
				words = append(words, "--"+flag.Name+"="+flag.Value.String())
			}
		})
		words = append(words, "--", query)
		var next string
		if !slices.ContainsFunc(words, func(word string) bool { return strings.ContainsAny(word, `'\`) }) {
			next = "docbank search '" + strings.Join(words, "' '") + "'"
		}
		hints = append(hints, searchHint{Code: "indexing_pending", Message: fmt.Sprintf("%d %s %s still being indexed; search again in a few seconds", coverage.Pending, noun, verb), Next: next})
	}
	if lexical && !fuzzy && len(strings.Fields(query)) > 1 {
		scope := ""
		if scoped {
			scope = " in this search scope"
		}
		hints = append(hints, searchHint{Code: "every_word_required", Message: fmt.Sprintf("no name or text segment%s matches all words; try fewer words if some match", scope)})
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
