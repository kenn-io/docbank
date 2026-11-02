package errtext

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}

func TestAnalyzerIncludeTests(t *testing.T) {
	IncludeTests = true
	t.Cleanup(func() { IncludeTests = false })
	analysistest.Run(t, analysistest.TestData(), Analyzer, "b")
}
