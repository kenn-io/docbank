// Command benchgate compares baseline and candidate Go benchmark output and
// fails when allocation or statistically significant time regressions exceed
// configured thresholds.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
	"golang.org/x/perf/benchunit"
)

const minTimeSamples = 5

type benchmarkSamples map[string]map[string][]float64

type metricGate struct {
	unit             string
	maxRatio         float64
	floor            float64
	needSignificance bool
}

type violation struct {
	name, unit      string
	old, new, ratio float64
	maxRatio        float64
}

func (v violation) String() string {
	class := benchunit.ClassOf(v.unit)
	return fmt.Sprintf("%s: %s regressed %.2fx (%s -> %s, limit %.2fx)",
		v.name, v.unit, v.ratio, benchunit.Scale(v.old, class),
		benchunit.Scale(v.new, class), v.maxRatio)
}

func parseBench(reader *benchfmt.Reader) (benchmarkSamples, []string, error) {
	result := make(benchmarkSamples)
	var syntax []string
	for reader.Scan() {
		switch item := reader.Result().(type) {
		case *benchfmt.Result:
			name := string(item.Name.Full())
			if pkg := item.GetConfig("pkg"); pkg != "" {
				name = pkg + "." + name
			}
			if result[name] == nil {
				result[name] = make(map[string][]float64)
			}
			for _, value := range item.Values {
				result[name][value.Unit] = append(result[name][value.Unit], value.Value)
			}
		case *benchfmt.SyntaxError:
			syntax = append(syntax, item.Error())
		}
	}
	return result, syntax, reader.Err()
}

func parseFile(path string) (benchmarkSamples, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	return parseBench(benchfmt.NewReader(f, path))
}

func center(values []float64) float64 {
	thresholds := benchmath.DefaultThresholds
	return benchmath.AssumeNothing.Summary(benchmath.NewSample(values, &thresholds), 0.95).Center
}

func evaluate(gate metricGate, oldValues, newValues []float64) (string, *violation, error) {
	if gate.needSignificance && len(oldValues) < minTimeSamples {
		return "", nil, fmt.Errorf("%s needs at least %d baseline samples, got %d",
			gate.unit, minTimeSamples, len(oldValues))
	}
	if gate.needSignificance && len(newValues) < minTimeSamples {
		return "", nil, fmt.Errorf("%s needs at least %d candidate samples, got %d",
			gate.unit, minTimeSamples, len(newValues))
	}
	oldCenter := center(oldValues)
	newCenter := center(newValues)
	class := benchunit.ClassOf(gate.unit)
	span := fmt.Sprintf("%s %s -> %s", gate.unit,
		benchunit.Scale(oldCenter, class), benchunit.Scale(newCenter, class))
	if oldCenter <= 0 || oldCenter < gate.floor {
		return span + " (below comparison floor)", nil, nil
	}
	ratio := newCenter / oldCenter
	detail := fmt.Sprintf("%s (%.2fx, limit %.2fx)", span, ratio, gate.maxRatio)
	significant := true
	if gate.needSignificance {
		thresholds := benchmath.DefaultThresholds
		comparison := benchmath.AssumeNothing.Compare(
			benchmath.NewSample(oldValues, &thresholds),
			benchmath.NewSample(newValues, &thresholds))
		significant = comparison.P < comparison.Alpha
		detail = fmt.Sprintf("%s (%.2fx, limit %.2fx, %s)", span, ratio, gate.maxRatio, comparison)
		if !significant {
			detail += " [not significant]"
		}
	}
	if ratio <= gate.maxRatio || !significant {
		return detail, nil, nil
	}
	return detail, &violation{
		unit: gate.unit, old: oldCenter, new: newCenter,
		ratio: ratio, maxRatio: gate.maxRatio,
	}, nil
}

func compare(old, next benchmarkSamples, gates []metricGate) ([]string, []violation, []string) {
	names := make([]string, 0, len(next))
	for name := range next {
		names = append(names, name)
	}
	sort.Strings(names)
	var report []string
	var violations []violation
	var issues []string
	for _, name := range names {
		oldUnits, exists := old[name]
		if !exists {
			report = append(report, name+": new benchmark; no baseline")
			continue
		}
		newUnits := next[name]
		gated := make(map[string]bool, len(gates))
		var parts []string
		for _, gate := range gates {
			gated[gate.unit] = true
			oldValues, oldOK := oldUnits[gate.unit]
			newValues, newOK := newUnits[gate.unit]
			switch {
			case !oldOK && !newOK:
				continue
			case !oldOK:
				parts = append(parts, gate.unit+" missing from baseline")
				issues = append(issues, name+": "+gate.unit+" missing from baseline")
			case !newOK:
				parts = append(parts, gate.unit+" missing from candidate")
				issues = append(issues, name+": "+gate.unit+" missing from candidate")
			default:
				detail, found, err := evaluate(gate, oldValues, newValues)
				parts = append(parts, detail)
				if err != nil {
					issues = append(issues, name+": "+err.Error())
				}
				if found != nil {
					found.name = name
					violations = append(violations, *found)
				}
			}
		}
		for unit := range newUnits {
			if !gated[unit] {
				parts = append(parts, unit+" reported only")
			}
		}
		sort.Strings(parts)
		report = append(report, name+": "+strings.Join(parts, ", "))
	}
	var removed []string
	for name := range old {
		if _, exists := next[name]; !exists {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		report = append(report, name+": missing from candidate")
		issues = append(issues, name+": benchmark missing from candidate")
	}
	return report, violations, issues
}

func defaultGates(maxTime, maxAllocs, maxBytes, timeFloorNs, allocFloor, bytesFloor float64) []metricGate {
	return []metricGate{
		{unit: "allocs/op", maxRatio: maxAllocs, floor: allocFloor},
		{unit: "B/op", maxRatio: maxBytes, floor: bytesFloor},
		{unit: "sec/op", maxRatio: maxTime, floor: timeFloorNs / 1e9, needSignificance: true},
	}
}

func run(oldPath, newPath string, gates []metricGate) (string, int) {
	old, oldSyntax, err := parseFile(oldPath)
	if err != nil {
		return fmt.Sprintf("benchgate: read baseline: %v\n", err), 2
	}
	next, nextSyntax, err := parseFile(newPath)
	if err != nil {
		return fmt.Sprintf("benchgate: read candidate: %v\n", err), 2
	}
	report, violations, issues := compare(old, next, gates)
	var out strings.Builder
	for _, line := range report {
		fmt.Fprintln(&out, line)
	}
	for _, syntax := range oldSyntax {
		fmt.Fprintf(&out, "benchgate: baseline syntax: %s\n", syntax)
	}
	for _, syntax := range nextSyntax {
		fmt.Fprintf(&out, "benchgate: candidate syntax: %s\n", syntax)
	}
	if len(violations) > 0 {
		fmt.Fprintf(&out, "benchgate: %d regression(s):\n", len(violations))
		for _, found := range violations {
			fmt.Fprintf(&out, "  %s\n", found)
		}
	}
	for _, issue := range issues {
		fmt.Fprintf(&out, "benchgate: configuration: %s\n", issue)
	}
	switch {
	case len(oldSyntax) > 0 || len(nextSyntax) > 0 || len(issues) > 0 || len(next) == 0:
		return out.String(), 2
	case len(violations) > 0:
		return out.String(), 1
	default:
		fmt.Fprintln(&out, "benchgate: no regressions beyond thresholds")
		return out.String(), 0
	}
}

func main() {
	oldPath := flag.String("old", "", "baseline benchmark output")
	newPath := flag.String("new", "", "candidate benchmark output")
	maxTime := flag.Float64("max-time-ratio", 2.0, "maximum significant sec/op ratio")
	maxAllocs := flag.Float64("max-alloc-ratio", 1.20, "maximum allocs/op ratio")
	maxBytes := flag.Float64("max-bytes-ratio", 1.25, "maximum B/op ratio")
	timeFloorNs := flag.Float64("time-floor-ns", 100_000, "minimum baseline ns/op")
	allocFloor := flag.Float64("alloc-floor", 8, "minimum baseline allocs/op")
	bytesFloor := flag.Float64("bytes-floor", 4096, "minimum baseline B/op")
	flag.Parse()
	if *oldPath == "" || *newPath == "" {
		fmt.Fprintln(os.Stderr, "benchgate: -old and -new are required")
		os.Exit(2)
	}
	out, code := run(*oldPath, *newPath,
		defaultGates(*maxTime, *maxAllocs, *maxBytes, *timeFloorNs, *allocFloor, *bytesFloor))
	fmt.Print(out)
	os.Exit(code)
}
