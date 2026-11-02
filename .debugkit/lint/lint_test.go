package lint_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/packages"

	"go.kenn.io/kit/lint"
)

// Analyzers that declare facts run, with their prerequisites, on every
// dependency of every linted package. SSA for the standard library and
// third-party modules dominated lint time, so only linted packages build it.
func TestAnalyzersBuildSSAOnlyForLintedPackages(t *testing.T) {
	require := require.New(t)
	gopath, err := filepath.Abs("testdata")
	require.NoError(err)
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  filepath.Join(gopath, "src", "app"),
		Env:  append(os.Environ(), "GOPATH="+gopath, "GO111MODULE=off", "GOWORK=off", "GOFLAGS="),
	}, "app")
	require.NoError(err)
	require.Zero(packages.PrintErrors(pkgs))

	graph, err := checker.Analyze(lint.Analyzers(), pkgs, nil)
	require.NoError(err)

	var ssaPackages []string
	for act := range graph.All() {
		require.NoError(act.Err, act.String())
		if act.Analyzer == buildssa.Analyzer {
			ssaPackages = append(ssaPackages, act.Package.PkgPath)
		}
		// rowserrcheck stays quiet only if dep.Count's scanner fact reached app.
		assert.Empty(t, act.Diagnostics, act.String())
	}
	assert.Equal(t, []string{"app"}, ssaPackages)
}
