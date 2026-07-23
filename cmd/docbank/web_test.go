package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/client"
)

func TestRunWebOpensAuthenticatedFragmentWithoutPrintingKey(t *testing.T) {
	c := client.New("http://127.0.0.1:43210", "private key")
	var opened string
	var out bytes.Buffer
	root := t.TempDir()
	err := runWeb(t.Context(), &out, root, c, false, func(_ context.Context, rawURL string) error {
		opened = rawURL
		return nil
	})
	require.NoError(t, err)
	assert.NotContains(t, out.String(), "private")
	assert.Equal(t, "opened Docbank web application at http://127.0.0.1:43210/\n", out.String())
	u, err := url.Parse(opened)
	require.NoError(t, err)
	assert.Equal(t, "file", u.Scheme)
	assert.Empty(t, u.Fragment)
	assert.NotContains(t, opened, "api_key")
	path := filepath.FromSlash(u.Path)
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, `\`)
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "api_key=private+key")
}

func TestRunWebNoBrowserExplicitlyPrintsAuthenticatedURL(t *testing.T) {
	c := client.New("http://127.0.0.1:43210", "private")
	var out bytes.Buffer
	err := runWeb(t.Context(), &out, t.TempDir(), c, true, func(context.Context, string) error {
		return errors.New("must not open")
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "#api_key=private")
}

func TestValidateWebLaunchURLRejectsCredentialsAndRemoteURLs(t *testing.T) {
	for _, rawURL := range []string{
		"https://127.0.0.1:43210/#api_key=private",
		"http://example.com/#api_key=private",
		"http://127.0.0.1:43210/",
		"file:///tmp/launch.html#api_key=private",
		"file:///tmp/launch.html?api_key=private",
	} {
		require.Error(t, validateWebLaunchURL(rawURL), rawURL)
	}
	assert.NoError(t, validateWebLaunchURL("file:///tmp/docbank-web-launch.html"))
}

func TestRunWebRejectsNonLoopbackClientBeforeOpeningOrPrinting(t *testing.T) {
	c := client.New("http://example.com:43210", "private")
	var out bytes.Buffer
	called := false
	err := runWeb(t.Context(), &out, t.TempDir(), c, false, func(context.Context, string) error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, called)
	assert.Empty(t, out.String())
}
