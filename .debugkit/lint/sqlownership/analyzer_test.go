package sqlownership

import (
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestOwnership(t *testing.T) {
	t.Parallel()
	for _, analyzer := range []*analysis.Analyzer{CloseAnalyzer, ErrAnalyzer} {
		t.Run(analyzer.Name, func(t *testing.T) {
			t.Parallel()
			analysistest.Run(t, analysistest.TestData(), analyzer, "transfer", "consumer")
		})
	}
}

func TestLeaks(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, analysistest.TestData(), CloseAnalyzer, "leaks")
}

func TestUncheckedScanner(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, analysistest.TestData(), ErrAnalyzer, "unchecked")
}
