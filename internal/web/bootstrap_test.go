package web

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestWriteBootstrapKeepsCredentialsOutOfLaunchURL(t *testing.T) {
	root := t.TempDir()
	authenticated := "http://127.0.0.1:43210/#api_key=private%20key"
	launchURL, err := WriteBootstrap(root, authenticated)
	require.NoError(t, err)
	assert.NotContains(t, launchURL, "private")
	assert.NotContains(t, launchURL, "api_key")

	parsed, err := url.Parse(launchURL)
	require.NoError(t, err)
	assert.Equal(t, "file", parsed.Scheme)
	assert.Empty(t, parsed.RawQuery)
	assert.Empty(t, parsed.Fragment)

	path := filepath.FromSlash(parsed.Path)
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, `\`)
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), authenticated)
	assert.Contains(t, string(raw), "location.replace")
	assert.NotContains(t, string(raw), `<a href=`)
}

func TestWriteBootstrapRejectsUnauthenticatedDestination(t *testing.T) {
	for _, destination := range []string{
		"https://127.0.0.1:43210/#api_key=private",
		"http://127.0.0.1:43210/",
		"",
	} {
		_, err := WriteBootstrap(t.TempDir(), destination)
		require.Error(t, err, destination)
	}
	_, err := WriteBootstrap("", "http://127.0.0.1:43210/#api_key=private")
	require.Error(t, err)
}

func TestFileURLContainsNoFragmentOrQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space and #.html")
	got := fileURL(path)
	parsed, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "file", parsed.Scheme)
	assert.Empty(t, parsed.Fragment)
	assert.Empty(t, parsed.RawQuery)
	assert.True(t, strings.HasSuffix(parsed.Path, "space and #.html"))
}

func TestFallbackRemovesCredentialFragment(t *testing.T) {
	raw, err := assetFS.ReadFile("fallback/assets/clear-fragment.js")
	require.NoError(t, err)
	assert.Contains(t, string(raw), "history.replaceState")
	index, err := assetFS.ReadFile("fallback/index.html")
	require.NoError(t, err)
	assert.Contains(t, string(index), `/assets/clear-fragment.js`)
}

func TestRemoveBootstrapRemovesCredentialHandoff(t *testing.T) {
	root := t.TempDir()
	_, err := WriteBootstrap(root, "http://127.0.0.1:43210/#api_key=private")
	require.NoError(t, err)
	require.NoError(t, RemoveBootstrap(root))
	require.NoDirExists(t, filepath.Join(root, launchDirName))
	require.NoError(t, RemoveBootstrap(root))
}
