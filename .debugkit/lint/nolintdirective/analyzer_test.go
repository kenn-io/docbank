package nolintdirective

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), New([]string{"sleeptest", "errtext", "deadlinetest"}), "a")
}
