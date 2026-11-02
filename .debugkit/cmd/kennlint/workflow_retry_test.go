package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowRetries(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := ".github"
	file := filepath.Join(dir, "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, []byte("jobs:\n  test:\n    steps:\n      - name: Download\n        shell: bash\n        run: npm ci\n"), 0o600))
	var output bytes.Buffer
	n, err := runWorkflowRetries(nil, &output)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, strings.HasPrefix(output.String(), filepath.ToSlash(file)+":6:14: job test, step Download: npm: wrap in kit-retry\n"))
	require.NoError(t, os.WriteFile(file, []byte("jobs: ["), 0o600))
	_, err = runWorkflowRetries([]string{dir}, &output)
	assert.ErrorContains(t, err, "ci.yml")
}
