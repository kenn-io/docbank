// Package lint owns the shared Go lint policy for kenn-io repositories: the
// custom go/analysis analyzers, the canonical golangci-lint configuration, and
// the module plugin that lets a custom golangci-lint build run them.
//
// Consumers normally do not import this package directly. They build a custom
// golangci-lint binary that includes go.kenn.io/kit/lint/gclplugin and render
// their .golangci.yml with the kennlint command in go.kenn.io/kit/cmd/kennlint.
package lint

import (
	"golang.org/x/tools/go/analysis"

	"go.kenn.io/kit/lint/errtext"
	"go.kenn.io/kit/lint/nohttpmux"
	"go.kenn.io/kit/lint/sleeptest"
	"go.kenn.io/kit/lint/sqlcheck"
	"go.kenn.io/kit/lint/sqlownership"
	"go.kenn.io/kit/lint/testifyhelper"
)

// Analyzers returns every kit analyzer in a stable order.
func Analyzers() []*analysis.Analyzer {
	return []*analysis.Analyzer{
		errtext.Analyzer,
		nohttpmux.Analyzer,
		sleeptest.Analyzer,
		sleeptest.DeadlineAnalyzer,
		sqlcheck.Analyzer,
		sqlownership.CloseAnalyzer,
		sqlownership.ErrAnalyzer,
		testifyhelper.Analyzer,
	}
}
