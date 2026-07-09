package update_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/docbank/internal/update"
)

// buildRelease returns a tar.gz containing a fake "docbank" binary and its
// sha256, using kit's DefaultAssetName so naming matches production. The
// asset targets the test's own runtime.GOOS/GOARCH: Options exposes no
// CheckOptions override, so Check() always resolves the platform from
// runtime.GOOS/GOARCH, and the fake asset must match whatever machine runs
// the test.
func buildRelease(t *testing.T, version string) (assetName string, archive []byte, sum string) {
	t.Helper()
	content := []byte("#!/bin/sh\necho docbank " + version + "\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "docbank", Mode: 0o755, Size: int64(len(content))}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	name := selfupdate.DefaultAssetName(selfupdate.AssetRequest{
		BinaryName: "docbank", Version: version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Extension: ".tar.gz",
	})
	h := sha256.Sum256(buf.Bytes())
	return name, buf.Bytes(), hex.EncodeToString(h[:])
}

// fakeReleaseServer serves a single kenn-io/docbank release at the given
// version over the exact URL shapes kit's selfupdate client requests: the
// "latest" redirect, the tag landing page it lands on (fetchLatestReleaseFromWeb
// requires a 200 there), a HEAD+GET download asset, and a SHA256SUMS file.
// It must be TLS: kit's AllowUnsignedChecksums path requires https base URLs.
func fakeReleaseServer(t *testing.T, version string) (ts *httptest.Server, checksum string) {
	t.Helper()
	asset, archive, sum := buildRelease(t, version)
	tag := "v" + version
	mux := http.NewServeMux()
	mux.HandleFunc("/kenn-io/docbank/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/kenn-io/docbank/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/kenn-io/docbank/releases/tag/"+tag, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/kenn-io/docbank/releases/download/"+tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/kenn-io/docbank/releases/download/"+tag+"/SHA256SUMS",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "%s  %s\n", sum, asset)
		})
	ts = httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts, sum
}

func newFakeClient(t *testing.T, ts *httptest.Server, currentVersion string) selfupdate.Client {
	t.Helper()
	c := update.NewClient(t.TempDir())
	c.GitHubWebBaseURL = ts.URL
	c.GitHubAPIBaseURL = ts.URL // fallback never used; keep it off the real network
	c.HTTPClient = ts.Client()  // trusts the test server's TLS certificate
	c.CurrentVersion = currentVersion
	return c
}

func TestUpdateInstallsFromFakeRelease(t *testing.T) {
	ts, sum := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "0.0.1")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Yes: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err, out.String())
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Contains(t, string(got), "9.9.9")
	assert.Contains(t, out.String(), "9.9.9")
	assert.Contains(t, out.String(), sum, "reported checksum must match the release's SHA256SUMS entry")
}

func TestUpdateCoordinatesDaemonUnderLaunchLock(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "0.0.1")
	root := t.TempDir()
	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var locked bool
	var order []string
	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Yes: true, Client: &c, Root: root, Destination: dest,
		WithLaunchLock: func(_ context.Context, gotRoot string, fn func() error) error {
			require.Equal(t, root, gotRoot)
			order = append(order, "lock")
			locked = true
			err := fn()
			locked = false
			order = append(order, "unlock")
			return err
		},
		Stop: func(_ context.Context, gotRoot string) (bool, error) {
			require.True(t, locked, "stop must happen while the launch lock is held")
			require.Equal(t, root, gotRoot)
			order = append(order, "stop")
			return true, nil
		},
		Start: func(_ context.Context, gotRoot string) error {
			require.True(t, locked, "restart must happen while the launch lock is held")
			require.Equal(t, root, gotRoot)
			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Contains(t, string(got), "9.9.9", "restart should happen after install")
			order = append(order, "start")
			return nil
		},
	})
	require.NoError(t, err, out.String())
	assert.Equal(t, []string{"lock", "stop", "start", "unlock"}, order)
	assert.Contains(t, out.String(), "daemon restarted")
}

func TestUpdateCheckOnly(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "0.0.1")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		CheckOnly: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "9.9.9")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got), "check-only must not install")
}

func TestUpdateAlreadyUpToDate(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "1.0.0")
	c := newFakeClient(t, ts, "1.0.0")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Yes: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "already up to date")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got), "up-to-date run must not install")
}

func TestUpdateDevBuildRequiresForce(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "dev")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Yes: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "pass --force")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got), "dev build without --force must not install")

	out.Reset()
	err = update.Run(t.Context(), &out, update.Options{
		Yes: true, Force: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err, out.String())
	got, err = os.ReadFile(dest)
	require.NoError(t, err)
	assert.Contains(t, string(got), "9.9.9", "dev build with --force must install")
}

func TestUpdateRequiresConfirmationWithoutYes(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "0.0.1")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confirmation required")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

// TestUpdateForcedDevBuildInstall verifies that --force on a dev build
// installs the latest release with complete download metadata even when a
// stale check cache is present: kit's Check bypasses the cache under Force,
// so the primed cache file must not short-circuit the install.
func TestUpdateForcedDevBuildInstall(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	cacheDir := t.TempDir()
	cache := struct {
		CheckedAt time.Time `json:"checked_at"`
		Version   string    `json:"version"`
	}{CheckedAt: time.Now().Add(-time.Minute), Version: "v9.9.9"}
	data, err := json.Marshal(cache)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "update_check.json"), data, 0o600))

	c := update.NewClient(cacheDir)
	c.GitHubWebBaseURL = ts.URL
	c.GitHubAPIBaseURL = ts.URL
	c.HTTPClient = ts.Client()
	c.CurrentVersion = "dev"

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err = update.Run(t.Context(), &out, update.Options{
		Yes: true, Force: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "download:", "refetch must repopulate download metadata before install")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Contains(t, string(got), "9.9.9")
}

func TestUpdateDeclinedConfirmation(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	c := newFakeClient(t, ts, "0.0.1")

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Client: &c, Root: t.TempDir(), Destination: dest,
		Confirm: func(string) (bool, error) { return false, nil },
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "aborted")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}
