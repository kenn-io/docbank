// Package gclplugin registers the kit analyzers as the "kennlint" module
// plugin for a custom golangci-lint build.
//
// Add it to .custom-gcl.yml:
//
//	version: v2.13.1
//	plugins:
//	  - module: go.kenn.io/kit
//	    import: go.kenn.io/kit/lint/gclplugin
//	    version: <kit version>
//
// and enable the linter in .golangci.yml under linters.settings.custom with
// type "module". Settings:
//
//	settings:
//	  disable: [deadlinetest]     # analyzer names to leave out
//	  errtext:
//	    include-tests: true       # also report err.Error() matching in tests
//	  sleeptest:
//	    helper-packages: false    # only check _test.go files (default true)
//	    eventually: true          # also report testify Eventually outside bubbles
//	  testifyhelper:
//	    require-helpers: true     # require assert.New/require.New (default false)
package gclplugin

import (
	"fmt"
	"slices"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"

	"go.kenn.io/kit/lint"
	"go.kenn.io/kit/lint/errtext"
	"go.kenn.io/kit/lint/nolintdirective"
	"go.kenn.io/kit/lint/sleeptest"
	"go.kenn.io/kit/lint/testifyhelper"
)

// Name is the linter name used in golangci-lint configuration.
const Name = "kennlint"

// Settings is the plugin configuration accepted under linters.settings.custom.kennlint.settings.
type Settings struct {
	// Disable lists analyzer names that should not run.
	Disable []string `json:"disable"`
	// Errtext configures the errtext analyzer.
	Errtext ErrtextSettings `json:"errtext"`
	// Sleeptest configures the sleeptest analyzer.
	Sleeptest SleeptestSettings `json:"sleeptest"`
	// Testifyhelper configures the testifyhelper analyzer.
	Testifyhelper TestifyhelperSettings `json:"testifyhelper"`
}

// TestifyhelperSettings configures the testifyhelper analyzer.
type TestifyhelperSettings struct {
	// RequireHelpers requires local assert and require helpers for repeated calls.
	// Canonical naming checks remain enabled when false (the default).
	RequireHelpers bool `json:"require-helpers"`
}

// ErrtextSettings configures the errtext analyzer.
type ErrtextSettings struct {
	IncludeTests bool `json:"include-tests"`
}

// SleeptestSettings configures the sleeptest analyzer.
type SleeptestSettings struct {
	// HelperPackages also checks packages named testutil or ending in
	// "test". nil means the analyzer default (true).
	HelperPackages *bool `json:"helper-packages"`
	// Eventually also reports testify Eventually, EventuallyWithT, and Never
	// outside a bubble.
	Eventually bool `json:"eventually"`
}

func init() {
	register.Plugin(Name, New)
	register.Plugin("kennnolint", func(any) (register.LinterPlugin, error) { return &directivePlugin{}, nil })
}

// New builds the plugin from decoded settings.
func New(conf any) (register.LinterPlugin, error) {
	settings, err := register.DecodeSettings[Settings](conf)
	if err != nil {
		return nil, fmt.Errorf("kennlint settings: %w", err)
	}
	known := lint.Analyzers()
	for _, name := range settings.Disable {
		if !slices.ContainsFunc(known, func(a *analysis.Analyzer) bool { return a.Name == name }) {
			return nil, fmt.Errorf("kennlint settings: unknown analyzer %q in disable", name)
		}
	}
	return &plugin{settings: settings}, nil
}

type plugin struct {
	settings Settings
}

func (p *plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	errtext.IncludeTests = p.settings.Errtext.IncludeTests
	sleeptest.HelperPackages = p.settings.Sleeptest.HelperPackages == nil || *p.settings.Sleeptest.HelperPackages
	sleeptest.Eventually = p.settings.Sleeptest.Eventually
	var analyzers []*analysis.Analyzer
	for _, a := range lint.Analyzers() {
		if slices.Contains(p.settings.Disable, a.Name) {
			continue
		}
		if a.Name == testifyhelper.Analyzer.Name {
			a = testifyhelper.New(p.settings.Testifyhelper.RequireHelpers)
		}
		analyzers = append(analyzers, a)
	}
	return analyzers, nil
}

func (p *plugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

type directivePlugin struct{}

func (*directivePlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	var names []string
	for _, a := range lint.Analyzers() {
		names = append(names, a.Name)
	}
	return []*analysis.Analyzer{nolintdirective.New(names)}, nil
}

func (*directivePlugin) GetLoadMode() string { return register.LoadModeTypesInfo }
