package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
)

func TestConfigureEmailPDFMissingAndInvalidRuntime(t *testing.T) {
	registry := processing.NewRenditionRuntimeRegistry()
	r, err := configureEmailPDF(t.Context(), config.Default(), nil, nil, t.TempDir(), t.TempDir(), registry)
	require.NoError(t, err)
	require.Nil(t, r)
	cfg := config.Default()
	cfg.EmailPDF = &config.EmailPDFConfig{Chromium: "/missing/chrome"}
	r, err = configureEmailPDF(t.Context(), cfg, nil, nil, t.TempDir(), t.TempDir(), registry)
	if runtime.GOOS == "linux" {
		require.Error(t, err)
		require.NotErrorIs(t, err, emailpdf.ErrUnavailable, "malformed Linux configuration must abort startup")
	} else {
		require.ErrorIs(t, err, emailpdf.ErrUnavailable, "unsupported optional rendering must not abort daemon startup")
	}
	require.Nil(t, r)
	require.False(t, registry.Ready())
}

func TestServeRenditionWorkersFollowEmailPDFAvailability(t *testing.T) {
	for _, test := range []struct {
		name                           string
		configured, unreachableManager bool
	}{
		{name: "unconfigured"},
		{name: "configured", configured: true},
		{name: "unreachable-manager", configured: true, unreachableManager: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			configured := test.configured
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			if test.unreachableManager {
				runtimeDir := t.TempDir()
				t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
				t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(runtimeDir, "missing-bus"))
			}
			// Neither a crash before the marker write nor an invalid marker may
			// prevent access to a vault. Unowned staging must remain untouched.
			layout := home.Layout{Root: root}
			require.NoError(t, layout.Ensure())
			for _, name := range []string{"email-pdf-markerless", "email-pdf-invalid"} {
				dir := filepath.Join(layout.EmailPDFSpoolDir(), name)
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "message.html"), []byte("synthetic interrupted render"), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(layout.EmailPDFSpoolDir(), "email-pdf-invalid", ".docbank-email-pdf-unit"), []byte("unrelated.service"), 0o600))
			if configured {
				bundle, fonts := t.TempDir(), t.TempDir()
				chromium := filepath.Join(bundle, "chrome")
				// A matching pin alone is insufficient: this fixture is not a
				// browser, so the actual startup render must disable rendering.
				require.NoError(t, os.WriteFile(chromium, []byte("synthetic pinned renderer"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(fonts, "font.ttf"), []byte("synthetic pinned font"), 0o600))
				bundleHash, err := emailpdf.TreeSHA256(bundle)
				require.NoError(t, err)
				fontsHash, err := emailpdf.TreeSHA256(fonts)
				require.NoError(t, err)
				configText := fmt.Sprintf("[email_pdf]\nchromium=%q\nbundle=%q\nbundle_sha256=%q\nversion=%q\nfonts=%q\nfonts_sha256=%q\n", chromium, bundle, bundleHash, "synthetic", fonts, fontsHash)
				require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(configText), 0o600))
			}
			startServe(t)
			record := waitForDaemon(t, root)
			client := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
			list, err := client.API().ListJobs(t.Context())
			require.NoError(t, err)
			var names []string
			for _, job := range list.Items {
				if strings.HasPrefix(job.Name, "process:renditions") {
					names = append(names, job.Name)
				}
			}
			want := []string{"process:renditions"}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+record.Address+"/api/v1/email-pdfs", strings.NewReader(`{"paper":"A3"}`))
			require.NoError(t, err)
			request.Header.Set("X-Api-Key", record.Metadata["api_key"])
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, string(body))
			require.Contains(t, string(body), "email_pdf_unavailable")
			if configured {
				require.Contains(t, string(body), "email PDF unavailable", "the configured prerequisite failure must reach the API")
			}
			t.Logf("optional rendering unavailable: %s", body)
			require.Equal(t, want, names)
			entries, err := os.ReadDir(layout.EmailPDFSpoolDir())
			require.NoError(t, err)
			require.Len(t, entries, 2, "confirmed probe cleanup must leave only the two unowned staging directories")
			for _, name := range []string{"email-pdf-markerless", "email-pdf-invalid"} {
				body, err := os.ReadFile(filepath.Join(layout.EmailPDFSpoolDir(), name, "message.html"))
				require.NoError(t, err)
				require.Equal(t, "synthetic interrupted render", string(body))
			}
		})
	}
}
