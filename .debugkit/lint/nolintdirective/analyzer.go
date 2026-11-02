// Package nolintdirective checks rule labels and reasons in kit suppressions.
package nolintdirective

import (
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// New checks suppressions against the registered kit analyzer names.
// It runs as a separate linter so nolint:kennlint cannot hide its diagnostics.
func New(names []string) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: "kennnolint",
		Doc:  "requires one or more known kit rules and a reason in nolint:kennlint explanations",
		Run: func(pass *analysis.Pass) (any, error) {
			for _, file := range pass.Files {
				for _, group := range file.Comments {
					for _, comment := range group.List {
						text, ok := strings.CutPrefix(strings.TrimLeft(comment.Text, "/ "), "nolint:")
						if !ok {
							continue
						}
						linters, reason, _ := strings.Cut(text, "//")
						if !slices.ContainsFunc(strings.Split(strings.TrimSpace(linters), ","), func(name string) bool { return strings.EqualFold(strings.TrimSpace(name), "kennlint") }) {
							continue
						}
						rules, explanation, ok := strings.Cut(strings.TrimSpace(reason), ":")
						unknownRule := slices.ContainsFunc(strings.Split(rules, ","), func(rule string) bool { return !slices.Contains(names, strings.TrimSpace(rule)) })
						if !ok || unknownRule || strings.TrimSpace(explanation) == "" {
							pass.Reportf(comment.Pos(), "nolint:kennlint must name a kit rule and a reason: //nolint:kennlint // <rule>[,<rule>...]: <reason>")
						}
					}
				}
			}
			return nil, nil
		},
	}
}
