package lint_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"go.kenn.io/kit/lint"
)

// BenchmarkAnalyzers runs each kit analyzer, with its prerequisites, over
// this module, the way golangci-lint does for a consumer: dependencies are
// analyzed from source when an analyzer needs facts. Compare runs with
// benchstat; kennlint profile breaks a consumer repository's run down.
func BenchmarkAnalyzers(b *testing.B) {
	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadAllSyntax, Tests: true, Dir: ".."}, "./...")
	require.NoError(b, err)
	require.Zero(b, packages.PrintErrors(pkgs))

	type set struct {
		name      string
		analyzers []*analysis.Analyzer
	}
	sets := []set{{"all", lint.Analyzers()}}
	for _, a := range lint.Analyzers() {
		sets = append(sets, set{a.Name, []*analysis.Analyzer{a}})
	}
	for _, s := range sets {
		analyzers := s.analyzers
		b.Run(s.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				// Sequential keeps one analyzer's time from absorbing
				// contention with the others.
				graph, err := checker.Analyze(analyzers, pkgs, &checker.Options{Sequential: true})
				require.NoError(b, err)
				for act := range graph.All() {
					require.NoError(b, act.Err, act.String())
				}
			}
		})
	}
}
