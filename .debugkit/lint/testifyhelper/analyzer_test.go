package testifyhelper

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := New(false)
	require.NoError(t, analyzer.Flags.Set("require-helpers", "true"))
	analysistest.Run(t, testdata, analyzer, "a")
}

func TestSuggestedFixes(t *testing.T) {
	t.Parallel()
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), New(true), "fixes", "aliases")
}

func TestDefaultAllowsPackageCalls(t *testing.T) {
	t.Parallel()
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), Analyzer, "optional", "aliases")
}
