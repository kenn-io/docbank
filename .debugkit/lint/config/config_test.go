package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type rendered struct {
	Linters struct {
		Enable   []string `yaml:"enable"`
		Settings struct {
			Importas struct {
				Alias []struct {
					Pkg   string `yaml:"pkg"`
					Alias string `yaml:"alias"`
				} `yaml:"alias"`
			} `yaml:"importas"`
			Forbidigo struct {
				Forbid []struct {
					Pattern string `yaml:"pattern"`
				} `yaml:"forbid"`
			} `yaml:"forbidigo"`
			Testifylint map[string]any `yaml:"testifylint"`
		} `yaml:"settings"`
		Exclusions struct {
			Rules []map[string]any `yaml:"rules"`
		} `yaml:"exclusions"`
	} `yaml:"linters"`
	Formatters struct {
		Enable []string `yaml:"enable"`
	} `yaml:"formatters"`
	Run struct {
		Timeout string `yaml:"timeout"`
	} `yaml:"run"`
}

func decode(t *testing.T, src []byte) rendered {
	t.Helper()
	var out rendered
	require.NoError(t, yaml.Unmarshal(src, &out))
	return out
}

func TestRenderWithoutOverlayIsCanonical(t *testing.T) {
	assert := assert.New(t)
	out, err := Render(nil)
	require.NoError(t, err)

	assert.True(strings.HasPrefix(string(out), Header))
	canonical := decode(t, Canonical)
	got := decode(t, out)
	assert.Equal(canonical, got)
	assert.Contains(got.Linters.Enable, "kennlint")
	assert.NotContains(got.Linters.Enable, "kennnolint")
	assert.Contains(got.Linters.Enable, "testifylint")
	assert.Equal([]string{"gofmt", "gofumpt"}, got.Formatters.Enable)
}

func TestRenderMergesOverlay(t *testing.T) {
	assert := assert.New(t)
	overlay := []byte(`
run:
  timeout: 20m
linters:
  enable:
    - gosec
    - errcheck
  disable:
    - nolintlint
  settings:
    importas:
      alias:
        - alias: ""
          pkg: github.com/stretchr/testify/assert
        - pkg: example.com/x/y
          alias: xy
    forbidigo:
      forbid:
        - pattern: '^db\.Open$'
          msg: Use fixtures.
    testifylint:
      disable:
        - float-compare
  exclusions:
    rules:
      - linters: [staticcheck]
        path: legacy/
        text: "ST1005:"
formatters:
  disable:
    - gofumpt
`)
	out, err := Render(overlay)
	require.NoError(t, err)
	got := decode(t, out)

	assert.Equal("20m", got.Run.Timeout)
	assert.Contains(got.Linters.Enable, "gosec")
	assert.NotContains(got.Linters.Enable, "nolintlint")
	assert.Equal(1, countOf(got.Linters.Enable, "errcheck"), "existing entries are not duplicated")
	assert.Equal([]string{"gofmt"}, got.Formatters.Enable)

	var aliasPkgs []string
	for _, a := range got.Linters.Settings.Importas.Alias {
		aliasPkgs = append(aliasPkgs, a.Pkg)
	}
	assert.Equal([]string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require", "example.com/x/y"}, aliasPkgs)

	var patterns []string
	for _, f := range got.Linters.Settings.Forbidigo.Forbid {
		patterns = append(patterns, f.Pattern)
	}
	assert.Contains(patterns, `^db\.Open$`)
	assert.Contains(patterns, `^t\.(Fatal|Fatalf|Error|Errorf|Fail|FailNow)$`)
	assert.Equal(map[string]any{"enable-all": true, "disable": []any{"encoded-compare", "float-compare"}}, got.Linters.Settings.Testifylint)
	assert.Equal("legacy/", got.Linters.Exclusions.Rules[len(got.Linters.Exclusions.Rules)-1]["path"])
	assert.NotContains(string(out), "disable:\n    - nolintlint")
}

func TestRenderRejectsMalformedOverlay(t *testing.T) {
	assert := assert.New(t)
	_, err := Render([]byte("- just\n- a list\n"))
	require.ErrorContains(t, err, "parsing overlay")

	_, err = Render([]byte("linters:\n  disable: nolintlint\n"))
	assert.ErrorContains(err, "linters.disable must be a list")
}

func TestRenderIsIdempotentForCanonical(t *testing.T) {
	first, err := Render(nil)
	require.NoError(t, err)
	second, err := Render(nil)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func countOf(items []string, want string) int {
	n := 0
	for _, item := range items {
		if item == want {
			n++
		}
	}
	return n
}
