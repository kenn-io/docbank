package gclplugin

import (
	"path/filepath"
	"testing"

	"github.com/golangci/plugin-module-register/register"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"

	"go.kenn.io/kit/lint/errtext"
	"go.kenn.io/kit/lint/sleeptest"
)

func analyzerNames(t *testing.T, p register.LinterPlugin) []string {
	t.Helper()
	analyzers, err := p.BuildAnalyzers()
	require.NoError(t, err)
	names := make([]string, 0, len(analyzers))
	for _, a := range analyzers {
		names = append(names, a.Name)
	}
	return names
}

func TestPluginIsRegistered(t *testing.T) {
	assert := assert.New(t)
	newPlugin, err := register.GetPlugin(Name)
	require.NoError(t, err)
	p, err := newPlugin(nil)
	require.NoError(t, err)
	assert.Equal([]string{"errtext", "nohttpmux", "sleeptest", "deadlinetest", "sqlcheck", "sqlclosecheck", "rowserrcheck", "testifyhelper"}, analyzerNames(t, p))
	assert.Equal(register.LoadModeTypesInfo, p.GetLoadMode())
	assert.False(errtext.IncludeTests)
	assert.True(sleeptest.HelperPackages)
	assert.False(sleeptest.Eventually)
}

func TestPluginSettings(t *testing.T) {
	assert := assert.New(t)
	t.Cleanup(func() {
		errtext.IncludeTests = false
		sleeptest.HelperPackages = true
		sleeptest.Eventually = false
	})
	p, err := New(map[string]any{
		"disable":   []any{"sleeptest", "nohttpmux", "deadlinetest"},
		"errtext":   map[string]any{"include-tests": true},
		"sleeptest": map[string]any{"helper-packages": false, "eventually": true},
	})
	require.NoError(t, err)
	assert.Equal([]string{"errtext", "sqlcheck", "sqlclosecheck", "rowserrcheck", "testifyhelper"}, analyzerNames(t, p))
	assert.True(errtext.IncludeTests)
	assert.False(sleeptest.HelperPackages)
	assert.True(sleeptest.Eventually)
}

func TestPluginRejectsUnknownSettings(t *testing.T) {
	assert := assert.New(t)
	_, err := New(map[string]any{"disable": []any{"nope"}})
	require.ErrorContains(t, err, `unknown analyzer "nope"`)

	_, err = New(map[string]any{"bogus": true})
	assert.ErrorContains(err, "kennlint settings")
}

func TestTestifyHelperSettings(t *testing.T) {
	testdata, err := filepath.Abs(filepath.Join("..", "testifyhelper", "testdata"))
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		settings any
		fixtures []string
	}{
		{name: "default", fixtures: []string{"optional", "aliases"}},
		{name: "disabled", settings: map[string]any{"testifyhelper": map[string]any{"require-helpers": false}}, fixtures: []string{"optional", "aliases"}},
		{name: "enabled", settings: map[string]any{"testifyhelper": map[string]any{"require-helpers": true}}, fixtures: []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.settings)
			require.NoError(t, err)
			analyzers, err := p.BuildAnalyzers()
			require.NoError(t, err)
			for _, a := range analyzers {
				if a.Name == "testifyhelper" {
					analysistest.Run(t, testdata, a, tc.fixtures...)
				}
			}
		})
	}
}

func TestDirectivePluginIsRegistered(t *testing.T) {
	newPlugin, err := register.GetPlugin("kennnolint")
	require.NoError(t, err)
	p, err := newPlugin(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"kennnolint"}, analyzerNames(t, p))
}
