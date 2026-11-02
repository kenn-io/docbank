package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileSplitsDependencyTime(t *testing.T) {
	require := require.New(t)
	gopath, err := filepath.Abs(filepath.Join("..", "..", "lint", "testdata"))
	require.NoError(err)
	t.Setenv("GOPATH", gopath)
	t.Setenv("GO111MODULE", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	t.Chdir(filepath.Join(gopath, "src", "app"))

	var stdout bytes.Buffer
	require.NoError(runProfile([]string{"-disable", "testifyhelper", "app"}, &stdout, &bytes.Buffer{}))
	out := stdout.String()

	assert := assert.New(t)
	assert.Regexp(`load: +1 packages, [1-9]\d* dependencies analyzed from source`, out)
	assert.Contains(out, "sqlscanners declare facts")
	// buildssa runs on the linted package only; the fact pass also covers dependencies.
	assert.Regexp(`(?m)^ *buildssa +\S+ +\S+ +- +0 +rowserrcheck, sqlclosecheck$`, out)
	assert.Regexp(`(?m)^ *sqlscanners +\S+ +\S+ +\S+ +[1-9]\d* `, out)
	assert.NotContains(out, "testifyhelper")
}

func TestProfileRejectsUnknownAnalyzer(t *testing.T) {
	err := runProfile([]string{"-disable", "nope"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorContains(t, err, `unknown analyzer "nope"`)
}
