package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"go.kenn.io/kit/lint"
)

// analyzerCost is the time one analyzer spent across a profiled run, split
// between the requested packages and the dependencies it ran on because an
// analyzer in the run consumes facts.
type analyzerCost struct {
	name              string
	rootTime, depTime time.Duration
	rootPkgs, depPkgs int
	requiredBy        []string
}

func runProfile(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("kennlint profile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tags := flags.String("tags", "", "comma-separated build tags, as in go build -tags")
	tests := flags.Bool("test", true, "also analyze test packages")
	disable := flags.String("disable", "", "comma-separated analyzer names to leave out")
	parallel := flags.Bool("parallel", false, "run analyzers in parallel; wall time matches a real run but per-analyzer times include contention")
	cpuProfile := flags.String("cpuprofile", "", "write a CPU profile of the analysis phase to this file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	analyzers, err := selectAnalyzers(*disable)
	if err != nil {
		return err
	}
	factAnalyzers := withFacts(analyzers)

	// Facts force analysis of every dependency, which needs their syntax.
	// Without facts, dependencies come from export data as in a normal build.
	mode := packages.LoadSyntax
	if len(factAnalyzers) > 0 {
		mode = packages.LoadAllSyntax
	}
	cfg := &packages.Config{Mode: mode, Tests: *tests}
	if *tags != "" {
		cfg.BuildFlags = []string{"-tags=" + *tags}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	allocStart := mem.TotalAlloc
	loadStart := time.Now()
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return fmt.Errorf("loading packages: %w", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return fmt.Errorf("loading packages: %d errors", n)
	}
	loadTime := time.Since(loadStart)
	runtime.ReadMemStats(&mem)
	loadAlloc := mem.TotalAlloc - allocStart

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return fmt.Errorf("creating CPU profile: %w", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return fmt.Errorf("starting CPU profile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}
	allocStart = mem.TotalAlloc
	analyzeStart := time.Now()
	graph, err := checker.Analyze(analyzers, pkgs, &checker.Options{Sequential: !*parallel})
	if err != nil {
		return fmt.Errorf("analyzing: %w", err)
	}
	analyzeTime := time.Since(analyzeStart)
	runtime.ReadMemStats(&mem)
	analyzeAlloc := mem.TotalAlloc - allocStart

	costs, rootPkgs, depPkgs := summarize(graph, analyzers)
	var failures []error
	for act := range graph.All() {
		if act.Err != nil && act.IsRoot {
			failures = append(failures, fmt.Errorf("%s: %w", act, act.Err))
		}
	}

	fmt.Fprintf(stdout, "load:     %d packages, %d dependencies analyzed from source, %s, %s allocated\n",
		rootPkgs, depPkgs, loadTime.Round(time.Millisecond), formatBytes(loadAlloc))
	fmt.Fprintf(stdout, "analysis: %s wall (%s), %s allocated\n",
		analyzeTime.Round(time.Millisecond), map[bool]string{true: "parallel", false: "sequential"}[*parallel], formatBytes(analyzeAlloc))
	if len(factAnalyzers) > 0 {
		fmt.Fprintf(stdout, "facts:    %s declare facts, so they and their prerequisites run on every dependency\n", strings.Join(factAnalyzers, ", "))
	}
	fmt.Fprintln(stdout)

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "analyzer\ttotal\tpackages\tdependencies\tdep pkgs\t\trequired by")
	for _, c := range costs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t\t%s\n", c.name,
			formatDuration(c.rootTime+c.depTime), formatDuration(c.rootTime), formatDuration(c.depTime), c.depPkgs,
			strings.Join(c.requiredBy, ", "))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return errors.Join(failures...)
}

func selectAnalyzers(disable string) ([]*analysis.Analyzer, error) {
	all := lint.Analyzers()
	var skip []string
	if disable != "" {
		skip = strings.Split(disable, ",")
	}
	for _, name := range skip {
		if !slices.ContainsFunc(all, func(a *analysis.Analyzer) bool { return a.Name == name }) {
			return nil, fmt.Errorf("unknown analyzer %q in -disable", name)
		}
	}
	return slices.DeleteFunc(all, func(a *analysis.Analyzer) bool { return slices.Contains(skip, a.Name) }), nil
}

// withFacts names the analyzers, including prerequisites, that declare facts.
func withFacts(analyzers []*analysis.Analyzer) []string {
	var names []string
	for a := range closure(analyzers) {
		if len(a.FactTypes) > 0 {
			names = append(names, a.Name)
		}
	}
	slices.Sort(names)
	return names
}

// closure returns every analyzer reachable through Requires, mapped to the
// requested analyzers that reach it.
func closure(analyzers []*analysis.Analyzer) map[*analysis.Analyzer][]string {
	reached := map[*analysis.Analyzer][]string{}
	var visit func(a *analysis.Analyzer, root string)
	visit = func(a *analysis.Analyzer, root string) {
		if slices.Contains(reached[a], root) {
			return
		}
		reached[a] = append(reached[a], root)
		for _, req := range a.Requires {
			visit(req, root)
		}
	}
	for _, a := range analyzers {
		visit(a, a.Name)
	}
	return reached
}

func summarize(graph *checker.Graph, analyzers []*analysis.Analyzer) (costs []*analyzerCost, rootPkgs, depPkgs int) {
	roots := map[*packages.Package]bool{}
	for _, act := range graph.Roots {
		roots[act.Package] = true
	}
	requiredBy := closure(analyzers)
	byAnalyzer := map[*analysis.Analyzer]*analyzerCost{}
	deps := map[*packages.Package]bool{}
	for act := range graph.All() {
		c := byAnalyzer[act.Analyzer]
		if c == nil {
			var by []string
			for _, name := range requiredBy[act.Analyzer] {
				if name != act.Analyzer.Name {
					by = append(by, name)
				}
			}
			slices.Sort(by)
			c = &analyzerCost{name: act.Analyzer.Name, requiredBy: by}
			byAnalyzer[act.Analyzer] = c
		}
		if roots[act.Package] {
			c.rootTime += act.Duration
			c.rootPkgs++
		} else {
			c.depTime += act.Duration
			c.depPkgs++
			deps[act.Package] = true
		}
	}
	for _, c := range byAnalyzer {
		costs = append(costs, c)
	}
	slices.SortFunc(costs, func(a, b *analyzerCost) int {
		return cmp.Or(cmp.Compare(b.rootTime+b.depTime, a.rootTime+a.depTime), cmp.Compare(a.name, b.name))
	})
	return costs, len(roots), len(deps)
}

func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

func formatBytes(n uint64) string {
	const mib = 1 << 20
	return fmt.Sprintf("%.0f MiB", float64(n)/mib)
}
