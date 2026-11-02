package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/perf/benchfmt"
)

func parseString(t *testing.T, input string) benchmarkSamples {
	t.Helper()
	result, syntax, err := parseBench(benchfmt.NewReader(strings.NewReader(input), "test"))
	require.NoError(t, err)
	require.Empty(t, syntax)
	return result
}

func TestParseBenchSeparatesPackagesAndSamples(t *testing.T) {
	result := parseString(t, "pkg: example/a\nBenchmarkRead-8 5 1000000 ns/op 10000 B/op 10 allocs/op\n"+
		"BenchmarkRead-8 5 1100000 ns/op 11000 B/op 11 allocs/op\n"+
		"pkg: example/b\nBenchmarkRead-8 5 2000000 ns/op 20000 B/op 20 allocs/op\n")
	require.Len(t, result, 2)
	assert.InDeltaSlice(t, []float64{0.001, 0.0011}, result["example/a.Read-8"]["sec/op"], 1e-12)
	assert.Equal(t, []float64{20}, result["example/b.Read-8"]["allocs/op"])
}

func TestCompareGatesDeterministicAndSignificantRegressions(t *testing.T) {
	old := benchmarkSamples{"Read-8": {
		"sec/op": {0.001, 0.00101, 0.00099, 0.00102, 0.00098},
		"B/op":   {10_000}, "allocs/op": {10},
	}}
	next := benchmarkSamples{"Read-8": {
		"sec/op": {0.004, 0.00401, 0.00399, 0.00402, 0.00398},
		"B/op":   {20_000}, "allocs/op": {20},
	}}
	_, violations, issues := compare(old, next, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Empty(t, issues)
	require.Len(t, violations, 3)
	assert.ElementsMatch(t, []string{"allocs/op", "B/op", "sec/op"},
		[]string{violations[0].unit, violations[1].unit, violations[2].unit})
}

func TestCompareReportsBenchmarkSetChanges(t *testing.T) {
	assert := assert.New(t)
	old := benchmarkSamples{"Old-8": {"sec/op": {0.001}}}
	next := benchmarkSamples{"New-8": {"sec/op": {0.001}}}
	report, violations, issues := compare(old, next, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Empty(violations)
	require.Len(t, issues, 1)
	assert.Contains(issues[0], "Old-8: benchmark missing from candidate")
	assert.Contains(strings.Join(report, "\n"), "new benchmark; no baseline")
	assert.Contains(strings.Join(report, "\n"), "missing from candidate")
}

func TestRunRejectsRemovedBenchmark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	baseline := filepath.Join(dir, "old.txt")
	candidate := filepath.Join(dir, "new.txt")
	line := "BenchmarkOld-8 5 1000000 ns/op 10000 B/op 10 allocs/op\n"
	require.NoError(os.WriteFile(baseline, []byte(strings.Repeat(line, 5)), 0o600))
	require.NoError(os.WriteFile(candidate, []byte(
		strings.Repeat(strings.ReplaceAll(line, "Old", "New"), 5)), 0o600))

	out, code := run(baseline, candidate, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Equal(2, code)
	assert.Contains(out, "Old-8: benchmark missing from candidate")
}

func TestCompareMissingCandidateMetricIsConfigurationError(t *testing.T) {
	old := benchmarkSamples{"Read-8": {"B/op": {10_000}, "allocs/op": {10}}}
	next := benchmarkSamples{"Read-8": {"allocs/op": {10}}}
	_, violations, issues := compare(old, next, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Empty(t, violations)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0], "B/op missing from candidate")
}

func TestCompareIncompleteBaselineIsConfigurationError(t *testing.T) {
	t.Run("missing metric", func(t *testing.T) {
		old := benchmarkSamples{"Read-8": {"sec/op": {0.001, 0.001, 0.001, 0.001, 0.001}}}
		next := benchmarkSamples{"Read-8": {
			"sec/op": {0.001, 0.001, 0.001, 0.001, 0.001}, "B/op": {10_000},
		}}
		_, violations, issues := compare(old, next, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
		assert.Empty(t, violations)
		require.Len(t, issues, 1)
		assert.Contains(t, issues[0], "B/op missing from baseline")
	})

	t.Run("too few timing samples", func(t *testing.T) {
		old := benchmarkSamples{"Read-8": {"sec/op": {0.000001}}}
		next := benchmarkSamples{"Read-8": {"sec/op": {0.001, 0.001, 0.001, 0.001, 0.001}}}
		_, violations, issues := compare(old, next, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
		assert.Empty(t, violations)
		require.Len(t, issues, 1)
		assert.Contains(t, issues[0], "needs at least 5 baseline samples")
	})
}

func TestRunRejectsMalformedBaseline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	baseline := filepath.Join(dir, "old.txt")
	candidate := filepath.Join(dir, "new.txt")
	require.NoError(os.WriteFile(baseline, []byte(
		"BenchmarkRead-8 not-a-valid-result\n"), 0o600))
	line := "BenchmarkRead-8 5 1000000 ns/op 10000 B/op 10 allocs/op\n"
	require.NoError(os.WriteFile(candidate, []byte(strings.Repeat(line, 5)), 0o600))

	out, code := run(baseline, candidate, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Equal(2, code)
	assert.Contains(out, "baseline syntax")
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	lines := func(ns, bytes, allocs int) string {
		line := fmt.Sprintf("BenchmarkRead-8 5 %d ns/op %d B/op %d allocs/op\n", ns, bytes, allocs)
		return strings.Repeat(line, 5)
	}
	baseline := write("old.txt", lines(1_000_000, 10_000, 10))
	passing := write("pass.txt", lines(1_100_000, 11_000, 11))
	regressed := write("fail.txt", lines(1_100_000, 30_000, 30))
	_, code := run(baseline, passing, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Zero(t, code)
	_, code = run(baseline, regressed, defaultGates(2, 1.2, 1.25, 100_000, 8, 4096))
	assert.Equal(t, 1, code)
}
