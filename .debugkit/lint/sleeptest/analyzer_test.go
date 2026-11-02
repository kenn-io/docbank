package sleeptest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a", "helperpkg")
}

func TestAnalyzerSkipsHelperPackagesWhenDisabled(t *testing.T) {
	HelperPackages = false
	t.Cleanup(func() { HelperPackages = true })
	// The fixture's want comments describe the default; with the flag off the
	// non-test file must produce nothing, which analysistest reports as
	// unmatched expectations, so check directly instead.
	results := analysistest.Run(&silentT{t}, analysistest.TestData(), Analyzer, "helperpkg")
	for _, r := range results {
		assert.Empty(t, r.Diagnostics, "helper-packages off")
	}
}

func TestAnalyzerReportsPollingAssertionsWhenEnabled(t *testing.T) {
	Eventually = true
	t.Cleanup(func() { Eventually = false })
	analysistest.Run(t, analysistest.TestData(), Analyzer, "poll")
}

func TestAnalyzerIgnoresPollingAssertionsByDefault(t *testing.T) {
	results := analysistest.Run(&silentT{t}, analysistest.TestData(), Analyzer, "poll")
	for _, r := range results {
		assert.Empty(t, r.Diagnostics, "eventually off")
	}
}

// silentT swallows analysistest's unmatched-expectation errors so a test can
// assert on the diagnostics directly.
type silentT struct{ *testing.T }

func (silentT) Errorf(string, ...any) {}

func TestDeadlineAnalyzer(t *testing.T) {
	analyzer := *DeadlineAnalyzer
	analyzer.Run = func(pass *analysis.Pass) (any, error) {
		// GOPATH fixtures have no module metadata; model a module and its subpackage.
		pass.Module = &analysis.Module{Path: "deadline"}
		return runDeadline(pass)
	}
	analysistest.Run(t, analysistest.TestData(), &analyzer, "deadline")
}
