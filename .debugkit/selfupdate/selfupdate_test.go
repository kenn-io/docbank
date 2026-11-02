package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHash64   = "abc123def456789012345678901234567890123456789012345678901234abcd"
	testHashAAAA = "abc123def456789012345678901234567890123456789012345678901234aaaa"
	testHashBBBB = "abc123def456789012345678901234567890123456789012345678901234bbbb"
)

func TestCheckFindsUpdateAndChecksumAsset(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	var checksumRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kenn/tool/releases/latest":
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Body:    "ignored",
				Assets: []Asset{
					{Name: "tool_1.2.0_linux_amd64.tar.gz", Size: 123, BrowserDownloadURL: "https://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "http://" + r.Host + "/SHA256SUMS"},
					{Name: "tool_1.2.0_linux_amd64.tar.gz.sha256.sig", BrowserDownloadURL: "https://example.invalid/tool.sig"},
				},
			})
		case "/SHA256SUMS":
			checksumRequests.Add(1)
			_, _ = fmt.Fprintf(w, "%s  tool_1.2.0_linux_amd64.tar.gz\n", testHash64)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		CacheDir:         t.TempDir(),
		GitHubAPIBaseURL: server.URL,
		Clock:            func() time.Time { return time.Unix(100, 0) },
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		require.FailNow(fmt.Sprintf("Check: %v", err))
	}
	if info == nil {
		require.FailNow("expected update info")
	}
	if info.CurrentVersion != "v1.1.0" || info.LatestVersion != "v1.2.0" {
		require.FailNow(fmt.Sprintf("unexpected versions: %+v", info))
	}
	if info.AssetName != "tool_1.2.0_linux_amd64.tar.gz" {
		require.FailNow(fmt.Sprintf("asset = %q", info.AssetName))
	}
	if info.SignatureURL != "https://example.invalid/tool.sig" {
		require.FailNow(fmt.Sprintf("signature URL = %q", info.SignatureURL))
	}
	if info.Checksum != testHash64 {
		require.FailNow(fmt.Sprintf("checksum = %q", info.Checksum))
	}
	if checksumRequests.Load() != 1 {
		require.FailNow(fmt.Sprintf("checksum requests = %d", checksumRequests.Load()))
	}
}

func TestCheckDiscoversReleaseThroughWebRedirectByDefault(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	var apiRequests atomic.Int64
	var latestPageRequests atomic.Int64
	var checksumRequests atomic.Int64
	assetName := "tool_1.2.0_linux_amd64.tar.gz"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/kenn/tool/releases/latest":
			apiRequests.Add(1)
			http.Error(w, "api should not be used before web discovery", http.StatusInternalServerError)
		case "GET /kenn/tool/releases/latest":
			latestPageRequests.Add(1)
			http.Redirect(w, r, "/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		case "GET /kenn/tool/releases/tag/v1.2.0":
			_, _ = w.Write([]byte("release page"))
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS":
			checksumRequests.Add(1)
			_, _ = fmt.Fprintf(w, "%s  %s\n", testHash64, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		CacheDir:         t.TempDir(),
		GitHubAPIBaseURL: server.URL,
		GitHubWebBaseURL: server.URL,
		Clock:            func() time.Time { return time.Unix(100, 0) },
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal("v1.2.0", info.LatestVersion)
	assert.Equal(assetName, info.AssetName)
	assert.Equal(server.URL+"/kenn/tool/releases/download/v1.2.0/"+assetName, info.DownloadURL)
	assert.Equal(testHash64, info.Checksum)
	assert.Equal(int64(123), info.Size)
	assert.Zero(apiRequests.Load())
	assert.Equal(int64(1), latestPageRequests.Load())
	assert.Equal(int64(1), checksumRequests.Load())
}

func TestCheckSkipsConventionalAssetProbeWhenWebTagIsCurrent(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	var assetProbeRequests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.Redirect(w, r, "/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		case "GET /kenn/tool/releases/tag/v1.2.0":
			_, _ = w.Write([]byte("release page"))
		case "HEAD /kenn/tool/releases/download/v1.2.0/tool_1.2.0_linux_amd64.tar.gz":
			assetProbeRequests.Add(1)
			http.Error(w, "already-current checks should not probe assets", http.StatusInternalServerError)
		case "GET /repos/kenn/tool/releases/latest":
			http.Error(w, "api fallback should not be needed", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.2.0",
		GitHubAPIBaseURL: server.URL,
		GitHubWebBaseURL: server.URL,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	assert.Nil(info)
	assert.Zero(assetProbeRequests.Load())
}

func TestCheckUsesReleaseManifestBeforeNetworkDiscovery(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	var apiRequests atomic.Int64
	var latestPageRequests atomic.Int64
	assetName := "tool_1.2.0_linux_amd64.tar.gz"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Assets: []Asset{
					{Name: assetName, Size: 123, BrowserDownloadURL: "https://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "https://" + r.Host + "/SHA256SUMS"},
				},
			})
		case "/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", testHash64, assetName)
		case "/repos/kenn/tool/releases/latest":
			apiRequests.Add(1)
			http.Error(w, "api should not be used when manifest is configured", http.StatusInternalServerError)
		case "/kenn/tool/releases/latest":
			latestPageRequests.Add(1)
			http.Error(w, "web discovery should not be used when manifest is configured", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:              "kenn",
		Repo:               "tool",
		BinaryName:         "tool",
		CurrentVersion:     "v1.1.0",
		ReleaseManifestURL: server.URL + "/latest.json",
		GitHubAPIBaseURL:   server.URL,
		GitHubWebBaseURL:   server.URL,
		HTTPClient:         server.Client(),
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal("v1.2.0", info.LatestVersion)
	assert.Equal(assetName, info.AssetName)
	assert.Equal("https://example.invalid/tool", info.DownloadURL)
	assert.Equal(testHash64, info.Checksum)
	assert.Zero(apiRequests.Load())
	assert.Zero(latestPageRequests.Load())
}

func TestCheckUsesManifestTagWithConventionalAssets(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	assetName := "tool_1.2.0_linux_amd64.tar.gz"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /latest.json":
			_ = json.NewEncoder(w).Encode(Release{TagName: "v1.2.0"})
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", testHash64, assetName)
		case "GET /repos/kenn/tool/releases/latest", "GET /kenn/tool/releases/latest":
			http.Error(w, "manifest should be enough", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:              "kenn",
		Repo:               "tool",
		BinaryName:         "tool",
		CurrentVersion:     "v1.1.0",
		ReleaseManifestURL: server.URL + "/latest.json",
		GitHubAPIBaseURL:   server.URL,
		GitHubWebBaseURL:   server.URL,
		HTTPClient:         server.Client(),
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal(assetName, info.AssetName)
	assert.Equal(server.URL+"/kenn/tool/releases/download/v1.2.0/"+assetName, info.DownloadURL)
	assert.Equal(testHash64, info.Checksum)
	assert.Equal(int64(123), info.Size)
}

func TestCheckRejectsHTTPManifestWhenUnsignedChecksumsAllowed(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "insecure manifest URL should be rejected before fetch")
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		ReleaseManifestURL:     server.URL + "/latest.json",
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "release manifest URL must use https")
}

func TestCheckRejectsHTTPManifest(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "insecure manifest URL should be rejected before fetch")
	}))
	defer server.Close()

	client := Client{
		Owner:              "kenn",
		Repo:               "tool",
		BinaryName:         "tool",
		CurrentVersion:     "v1.1.0",
		ReleaseManifestURL: server.URL + "/latest.json",
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "release manifest URL must use https")
}

func TestCheckRejectsHTTPManifestAssetWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Assets: []Asset{
					{Name: assetName, Size: 123, BrowserDownloadURL: "http://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "https://example.invalid/SHA256SUMS"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		ReleaseManifestURL:     server.URL + "/latest.json",
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "release asset URL for "+assetName+" must use https")
}

func TestCheckRejectsHTTPSManifestRedirectToHTTPWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/latest.json", http.StatusFound)
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		ReleaseManifestURL:     server.URL + "/latest.json",
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckRejectsHTTPSManifestRedirectToHTTP(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/latest.json", http.StatusFound)
	}))
	defer server.Close()

	client := Client{
		Owner:              "kenn",
		Repo:               "tool",
		BinaryName:         "tool",
		CurrentVersion:     "v1.1.0",
		ReleaseManifestURL: server.URL + "/latest.json",
		HTTPClient:         server.Client(),
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckRejectsHTTPSChecksumRedirectToHTTPWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Assets: []Asset{
					{Name: assetName, Size: 123, BrowserDownloadURL: "https://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "https://" + r.Host + "/SHA256SUMS"},
				},
			})
		case "/SHA256SUMS":
			http.Redirect(w, r, "http://example.invalid/SHA256SUMS", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		ReleaseManifestURL:     server.URL + "/latest.json",
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckUsesConventionalChecksumAndSignatureFallbacks(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	var primaryChecksumRequests atomic.Int64
	var fallbackChecksumRequests atomic.Int64

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /latest.json":
			_ = json.NewEncoder(w).Encode(Release{TagName: "v1.2.0"})
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName + ".sha256.sig":
			http.NotFound(w, r)
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName + ".sig":
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS":
			primaryChecksumRequests.Add(1)
			http.NotFound(w, r)
		case "GET /kenn/tool/releases/download/v1.2.0/checksums.txt":
			fallbackChecksumRequests.Add(1)
			_, _ = fmt.Fprintf(w, "%s  %s\n", testHash64, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:              "kenn",
		Repo:               "tool",
		BinaryName:         "tool",
		CurrentVersion:     "v1.1.0",
		ReleaseManifestURL: server.URL + "/latest.json",
		GitHubAPIBaseURL:   server.URL,
		GitHubWebBaseURL:   server.URL,
		HTTPClient:         server.Client(),
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal(testHash64, info.Checksum)
	assert.Equal(server.URL+"/kenn/tool/releases/download/v1.2.0/"+assetName+".sig", info.SignatureURL)
	assert.Equal(int64(1), primaryChecksumRequests.Load())
	assert.Equal(int64(1), fallbackChecksumRequests.Load())
}

func TestCheckFallsBackToAPIWhenWebConventionalReleaseHasNoChecksum(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	var apiRequests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.Redirect(w, r, "/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		case "GET /kenn/tool/releases/tag/v1.2.0":
			_, _ = w.Write([]byte("release page"))
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS", "GET /kenn/tool/releases/download/v1.2.0/checksums.txt":
			http.NotFound(w, r)
		case "GET /repos/kenn/tool/releases/latest":
			apiRequests.Add(1)
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Body:    fmt.Sprintf("%s  %s\n", testHash64, assetName),
				Assets: []Asset{
					{Name: assetName, Size: 456, BrowserDownloadURL: "https://example.invalid/tool"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		GitHubAPIBaseURL: server.URL,
		GitHubWebBaseURL: server.URL,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal(testHash64, info.Checksum)
	assert.Equal("https://example.invalid/tool", info.DownloadURL)
	assert.Equal(int64(456), info.Size)
	assert.Equal(int64(1), apiRequests.Load())
}

func TestCheckRejectsHTTPAPIAssetAfterWebChecksumFallbackWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	var apiRequests atomic.Int64

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.Redirect(w, r, "/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		case "GET /kenn/tool/releases/tag/v1.2.0":
			_, _ = w.Write([]byte("release page"))
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS", "GET /kenn/tool/releases/download/v1.2.0/checksums.txt":
			http.NotFound(w, r)
		case "GET /repos/kenn/tool/releases/latest":
			apiRequests.Add(1)
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Body:    fmt.Sprintf("%s  %s\n", testHash64, assetName),
				Assets: []Asset{
					{Name: assetName, Size: 456, BrowserDownloadURL: "http://example.invalid/tool"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		GitHubAPIBaseURL:       server.URL,
		GitHubWebBaseURL:       server.URL,
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(info)
	assert.Contains(err.Error(), "release asset URL for "+assetName+" must use https")
	assert.Equal(int64(1), apiRequests.Load())
}

func TestCheckRejectsHTTPWebBaseWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		GitHubWebBaseURL:       "http://example.invalid",
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "GitHub web base URL must use https")
}

func TestCheckRejectsWebChecksumHTTPRedirectWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	assetName := "tool_1.2.0_linux_amd64.tar.gz"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.Redirect(w, r, "/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		case "GET /kenn/tool/releases/tag/v1.2.0":
			_, _ = w.Write([]byte("release page"))
		case "HEAD /kenn/tool/releases/download/v1.2.0/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "GET /kenn/tool/releases/download/v1.2.0/SHA256SUMS":
			http.Redirect(w, r, "http://example.invalid/SHA256SUMS", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		GitHubAPIBaseURL:       server.URL,
		GitHubWebBaseURL:       server.URL,
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckRejectsWebLatestHTTPRedirectWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.Redirect(w, r, "http://example.invalid/kenn/tool/releases/tag/v1.2.0", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		GitHubAPIBaseURL:       server.URL,
		GitHubWebBaseURL:       server.URL,
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckSendsTokenOnlyToAPIFallback(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	require := require.New(t)
	assetName := "tool_1.2.0_linux_amd64.tar.gz"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			assert.Empty(r.Header.Get("Authorization"))
			http.Error(w, "web discovery unavailable", http.StatusInternalServerError)
		case "GET /repos/kenn/tool/releases/latest":
			assert.Equal("Bearer test-token", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Assets: []Asset{
					{Name: assetName, Size: 123, BrowserDownloadURL: "https://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "https://" + r.Host + "/SHA256SUMS"},
				},
			})
		case "GET /SHA256SUMS":
			assert.Empty(r.Header.Get("Authorization"))
			_, _ = fmt.Fprintf(w, "%s  %s\n", testHash64, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		GitHubAPIBaseURL: server.URL,
		GitHubWebBaseURL: server.URL,
		HTTPClient:       server.Client(),
		GitHubToken:      "test-token",
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.NoError(err)
	require.NotNil(info)
	assert.Equal(testHash64, info.Checksum)
}

func TestCheckRejectsTokenWithHTTPAPIBaseURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kenn/tool/releases/latest":
			http.Error(w, "web discovery unavailable", http.StatusInternalServerError)
		case "/repos/kenn/tool/releases/latest":
			assert.Fail(t, "token-bearing API request should be rejected before fetch")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		GitHubWebBaseURL: server.URL,
		GitHubAPIBaseURL: server.URL,
		GitHubToken:      "test-token",
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "GitHub API base URL must use https")
}

func TestCheckRejectsTokenAPIHTTPRedirect(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kenn/tool/releases/latest":
			http.Error(w, "web discovery unavailable", http.StatusInternalServerError)
		case "/repos/kenn/tool/releases/latest":
			assert.Equal("Bearer test-token", r.Header.Get("Authorization"))
			http.Redirect(w, r, "http://example.invalid/repos/kenn/tool/releases/latest", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		GitHubWebBaseURL: server.URL,
		GitHubAPIBaseURL: server.URL,
		HTTPClient:       server.Client(),
		GitHubToken:      "test-token",
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(info)
	assert.Contains(err.Error(), "redirect to non-HTTPS URL")
}

func TestCheckRejectsUnsignedAPIHTTPRedirectWithoutToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /kenn/tool/releases/latest":
			http.NotFound(w, r)
		case "GET /repos/kenn/tool/releases/latest":
			http.Redirect(w, r, "http://example.invalid/repos/kenn/tool/releases/latest", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:                  "kenn",
		Repo:                   "tool",
		BinaryName:             "tool",
		CurrentVersion:         "v1.1.0",
		GitHubAPIBaseURL:       server.URL,
		GitHubWebBaseURL:       server.URL,
		HTTPClient:             server.Client(),
		AllowUnsignedChecksums: true,
	}

	info, err := client.Check(t.Context(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestEnvironmentGitHubToken(t *testing.T) {
	assert := assert.New(t)

	t.Setenv("GH_TOKEN", "primary")
	t.Setenv("GITHUB_TOKEN", "fallback")
	assert.Equal("primary", EnvironmentGitHubToken())

	t.Setenv("GH_TOKEN", "")
	assert.Equal("fallback", EnvironmentGitHubToken())

	t.Setenv("GITHUB_TOKEN", "")
	assert.Empty(EnvironmentGitHubToken())
}

func TestCheckUsesReleaseBodyChecksumFallback(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Release{
			TagName: "v1.2.0",
			Body:    testHashAAAA + "  custom.tgz",
			Assets: []Asset{
				{Name: "custom.tgz", Size: 55, BrowserDownloadURL: "https://example.invalid/custom"},
			},
		})
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "dev",
		GitHubAPIBaseURL: server.URL,
		AssetName: func(AssetRequest) string {
			return "custom.tgz"
		},
	}

	info, err := client.Check(t.Context(), CheckOptions{})
	if err != nil {
		require.FailNow(fmt.Sprintf("Check: %v", err))
	}
	if info == nil {
		require.FailNow("expected update info")
	}
	if info.Checksum != testHashAAAA {
		require.FailNow(fmt.Sprintf("checksum = %q", info.Checksum))
	}
	if !info.IsDevBuild {
		require.FailNow("expected dev build")
	}
}

func TestCheckCache(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	tests := []struct {
		name           string
		currentVersion string
		isDevBuild     bool
		cachedVersion  string
		cacheAge       time.Duration
		wantInfo       bool
		wantDone       bool
		wantCacheOnly  bool
	}{
		{
			name:           "valid cache no update available",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.0.0",
			cacheAge:       30 * time.Minute,
			wantDone:       true,
		},
		{
			name:           "valid cache update available triggers fresh fetch",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.1.0",
			cacheAge:       30 * time.Minute,
			wantDone:       false,
		},
		{
			name:           "dev build returns cache-only update info",
			currentVersion: "0.16.1-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       5 * time.Minute,
			wantInfo:       true,
			wantDone:       true,
			wantCacheOnly:  true,
		},
		{
			name:           "parseable dev build at cached release does not downgrade",
			currentVersion: "v1.0.0-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       5 * time.Minute,
			wantDone:       true,
		},
		{
			name:           "expired cache",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.0.0",
			cacheAge:       2 * time.Hour,
			wantDone:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			t.Parallel()
			cacheDir := t.TempDir()
			data, err := json.Marshal(cachedCheck{
				CheckedAt: now.Add(-tt.cacheAge),
				Version:   tt.cachedVersion,
			})
			if err != nil {
				require.FailNow(err.Error())
			}
			if err := os.WriteFile(filepath.Join(cacheDir, defaultCacheFileName), data, 0o600); err != nil {
				require.FailNow(err.Error())
			}
			c := Client{
				BinaryName: "tool",
				CacheDir:   cacheDir,
				Clock:      func() time.Time { return now },
			}
			cleanVersion := strings.TrimPrefix(tt.currentVersion, "v")
			info, done := c.checkCache(tt.currentVersion, cleanVersion, tt.isDevBuild)
			if done != tt.wantDone {
				require.FailNow(fmt.Sprintf("done = %v, want %v", done, tt.wantDone))
			}
			if (info != nil) != tt.wantInfo {
				require.FailNow(fmt.Sprintf("info nil = %v, wantInfo %v", info == nil, tt.wantInfo))
			}
			if info != nil && info.NeedsRefetch() != tt.wantCacheOnly {
				require.FailNow(fmt.Sprintf("NeedsRefetch = %v, want %v", info.NeedsRefetch(), tt.wantCacheOnly))
			}
		})
	}
}

func TestSaveCacheFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions not enforced on Windows")
	}
	t.Parallel()

	cacheDir := t.TempDir()
	c := Client{
		CacheDir: cacheDir,
		Clock:    func() time.Time { return time.Unix(1, 0) },
	}
	if err := c.saveCache("v1.0.0"); err != nil {
		require.FailNow(t, fmt.Sprintf("saveCache: %v", err))
	}
	info, err := os.Stat(filepath.Join(cacheDir, defaultCacheFileName))
	if err != nil {
		require.FailNow(t, err.Error())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		require.FailNow(t, fmt.Sprintf("cache file mode = %04o, want 0600", got))
	}
}

func TestInstallDownloadsVerifiesAndInstalls(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	binaryName := "tool"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "tool_1.2.0_linux_amd64.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{{Name: binaryName, Content: "new-binary", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		require.FailNow(err.Error())
	}
	payload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.2.0",
		Asset:    filepath.Base(archivePath),
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	publicKey, signature := signPayload(t, payload)
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		require.FailNow(err.Error())
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(archiveBytes)
		case "/archive.sig":
			_, _ = w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dstPath := filepath.Join(tmpDir, binaryName)
	var lastProgress int64
	c := Client{
		Owner:             "kenn",
		Repo:              "tool",
		BinaryName:        "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
	}
	err = c.Install(t.Context(), &Info{
		DownloadURL:   server.URL + "/archive",
		SignatureURL:  server.URL + "/archive.sig",
		AssetName:     filepath.Base(archivePath),
		LatestVersion: "v1.2.0",
		Size:          int64(len(archiveBytes)),
		Checksum:      checksum,
	}, InstallOptions{
		DestinationPath: dstPath,
		Progress: func(downloaded, total int64) {
			lastProgress = downloaded
			if total != int64(len(archiveBytes)) {
				require.FailNow(fmt.Sprintf("progress total = %d", total))
			}
		},
	})
	if err != nil {
		require.FailNow(fmt.Sprintf("Install: %v", err))
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		require.FailNow(err.Error())
	}
	if string(got) != "new-binary" {
		require.FailNow(fmt.Sprintf("installed content = %q", got))
	}
	if lastProgress != int64(len(archiveBytes)) {
		require.FailNow(fmt.Sprintf("last progress = %d", lastProgress))
	}
}

func TestInstallRefusesUnverifiedOrCachedInfo(t *testing.T) {
	t.Parallel()

	c := Client{BinaryName: "tool"}
	if err := c.Install(t.Context(), &Info{AssetName: "tool.tar.gz"}, InstallOptions{}); err == nil {
		require.FailNow(t, "expected missing checksum error")
	}
	if err := c.Install(t.Context(), &Info{cacheOnly: true}, InstallOptions{}); err == nil {
		require.FailNow(t, "expected cache-only error")
	}
}

func TestInstallArchiveRequiresSignatureByDefault(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "tool.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		require.FailNow(err.Error())
	}
	dstPath := filepath.Join(tmpDir, "dest", "tool")
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		require.FailNow(err.Error())
	}
	if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{}); err != nil {
		if !strings.Contains(err.Error(), "requires a trusted public key") {
			require.FailNow(fmt.Sprintf("error = %v", err))
		}
	} else {
		require.FailNow("expected missing signature verification error")
	}
	if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{AllowUnsignedChecksums: true}); err != nil {
		require.FailNow(fmt.Sprintf("InstallArchive: %v", err))
	}
}

func TestInstallRequiresSignatureBeforeArchiveDownloadByDefault(t *testing.T) {
	t.Parallel()

	var archiveRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveRequests.Add(1)
		_, _ = w.Write([]byte("archive"))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	err := c.Install(t.Context(), &Info{
		LatestVersion: "v1.0.0",
		DownloadURL:   server.URL,
		AssetName:     "tool.tar.gz",
		Checksum:      strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "trusted public key is required") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
	if archiveRequests.Load() != 0 {
		require.FailNow(t, "archive was downloaded before signature verification")
	}
}

func TestInstallRejectsUnsafeAssetName(t *testing.T) {
	t.Parallel()

	c := Client{BinaryName: "tool"}
	err := c.Install(t.Context(), &Info{
		DownloadURL: "https://example.invalid/archive",
		AssetName:   "../outside.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "invalid asset name") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
}

func TestInstallVerifiesSignatureBeforeArchiveDownload(t *testing.T) {
	t.Parallel()

	var archiveRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			archiveRequests.Add(1)
			_, _ = w.Write([]byte("archive"))
		case "/archive.sig":
			_, _ = w.Write([]byte("not-a-signature"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	c := Client{
		Owner:             "kenn",
		Repo:              "tool",
		BinaryName:        "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
	}
	err = c.Install(t.Context(), &Info{
		LatestVersion: "v1.0.0",
		DownloadURL:   server.URL + "/archive",
		SignatureURL:  server.URL + "/archive.sig",
		AssetName:     "tool.tar.gz",
		Checksum:      strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "invalid format") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
	if archiveRequests.Load() != 0 {
		require.FailNow(t, "archive was downloaded before signature verification")
	}
}

func TestInstallRejectsMismatchedInfoRepository(t *testing.T) {
	t.Parallel()

	c := Client{
		Owner:      "kenn",
		Repo:       "tool",
		BinaryName: "tool",
	}
	err := c.Install(t.Context(), &Info{
		Owner:       "other",
		Repo:        "tool",
		DownloadURL: "https://example.invalid/archive",
		AssetName:   "tool.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "does not match client owner") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
}

func TestInstallRejectsDownloadLargerThanExpected(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("too large"))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool", HTTPClient: server.Client(), AllowUnsignedChecksums: true}
	err := c.Install(t.Context(), &Info{
		DownloadURL: server.URL,
		AssetName:   "tool.tar.gz",
		Size:        3,
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "exceeded expected size") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
}

func TestInstallRejectsArchiveHTTPRedirectWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/archive.tar.gz", http.StatusFound)
	}))
	defer server.Close()

	c := Client{BinaryName: "tool", HTTPClient: server.Client(), AllowUnsignedChecksums: true}
	err := c.Install(t.Context(), &Info{
		DownloadURL: server.URL + "/archive.tar.gz",
		AssetName:   "tool.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
}

func TestInstallRejectsHTTPArchiveBeforeRequestWhenUnsignedChecksumsAllowed(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("archive"))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool", AllowUnsignedChecksums: true}
	err := c.Install(t.Context(), &Info{
		DownloadURL: server.URL + "/archive.tar.gz",
		AssetName:   "tool.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect to non-HTTPS URL")
	assert.Equal(t, int64(0), requests.Load())
}

func TestInstallArchive(t *testing.T) {
	t.Parallel()

	t.Run("zip happy path with nested binary", func(t *testing.T) {
		require := require.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")
		createZip(t, archivePath, []archiveEntry{
			{Name: "tool-v1.0.0/tool", Content: "zip-binary", Mode: 0o755},
			{Name: "README.md", Content: "readme", Mode: 0o644},
		})
		checksum, err := HashFile(archivePath)
		if err != nil {
			require.FailNow(err.Error())
		}
		payload := SignaturePayload(SignatureMetadata{
			Version:  "v1.0.0",
			Asset:    "tool",
			GOOS:     runtime.GOOS,
			GOARCH:   runtime.GOARCH,
			Checksum: checksum,
		})
		publicKey, signature := signPayload(t, payload)

		dstPath := filepath.Join(tmpDir, "dest", "tool")
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{
			ArchiveBinaryName: "tool",
			TrustedPublicKeys: []ed25519.PublicKey{publicKey},
			ChecksumSignature: signature,
			SignaturePayload:  payload,
		}); err != nil {
			require.FailNow(fmt.Sprintf("InstallArchive: %v", err))
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			require.FailNow(err.Error())
		}
		if string(got) != "zip-binary" {
			require.FailNow(fmt.Sprintf("content = %q", got))
		}
	})

	t.Run("tar.gz checksum mismatch", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
		err := InstallArchive(archivePath, strings.Repeat("0", 64), filepath.Join(tmpDir, "tool"), InstallArchiveOptions{})
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			require.FailNow(t, fmt.Sprintf("error = %v", err))
		}
	})

	t.Run("walks past top-level directory named binary", func(t *testing.T) {
		require := require.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "nested.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "tool/tool", Content: "nested-binary", Mode: 0o755}})
		checksum, err := HashFile(archivePath)
		if err != nil {
			require.FailNow(err.Error())
		}

		dstPath := filepath.Join(tmpDir, "dest", "tool")
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{
			ArchiveBinaryName:      "tool",
			AllowUnsignedChecksums: true,
		}); err != nil {
			require.FailNow(fmt.Sprintf("InstallArchive: %v", err))
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			require.FailNow(err.Error())
		}
		if string(got) != "nested-binary" {
			require.FailNow(fmt.Sprintf("content = %q", got))
		}
	})
}

func TestInstallArchiveRejectsReplaySignature(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.zip")
	createZip(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		require.FailNow(t, err.Error())
	}

	oldPayload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.0.0",
		Asset:    "tool",
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	newPayload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.1.0",
		Asset:    "tool",
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	publicKey, signature := signPayload(t, oldPayload)

	err = InstallArchive(archivePath, checksum, filepath.Join(tmpDir, "tool"), InstallArchiveOptions{
		ArchiveBinaryName: "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
		ChecksumSignature: signature,
		SignaturePayload:  newPayload,
	})
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		require.FailNow(t, fmt.Sprintf("error = %v", err))
	}
}

func TestExtractTarGzAndZipRejectTraversal(t *testing.T) {
	t.Parallel()

	t.Run("tar.gz traversal", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "malicious.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "../pwned", Content: "owned", Mode: 0o644}})
		err := ExtractTarGz(archivePath, filepath.Join(tmpDir, "extract"))
		if err == nil {
			require.FailNow(t, "expected traversal error")
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "pwned")); !os.IsNotExist(err) {
			require.FailNow(t, fmt.Sprintf("outside file exists or stat failed unexpectedly: %v", err))
		}
	})

	t.Run("zip traversal", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "malicious.zip")
		createZip(t, archivePath, []archiveEntry{{Name: "../pwned", Content: "owned", Mode: 0o644}})
		err := ExtractZip(archivePath, filepath.Join(tmpDir, "extract"))
		if err == nil {
			require.FailNow(t, "expected traversal error")
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "pwned")); !os.IsNotExist(err) {
			require.FailNow(t, fmt.Sprintf("outside file exists or stat failed unexpectedly: %v", err))
		}
	})
}

func TestExtractArchivesRejectPreexistingSymlinkPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}

	t.Run("tar.gz", func(t *testing.T) {
		require := require.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "symlink-path.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "link/payload", Content: "owned", Mode: 0o644}})
		extractDir := filepath.Join(tmpDir, "extract")
		outsideDir := filepath.Join(tmpDir, "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := os.Symlink(outsideDir, filepath.Join(extractDir, "link")); err != nil {
			require.FailNow(err.Error())
		}
		if err := ExtractTarGz(archivePath, extractDir); err == nil {
			require.FailNow("expected symlink path error")
		}
		if _, err := os.Stat(filepath.Join(outsideDir, "payload")); !os.IsNotExist(err) {
			require.FailNow(fmt.Sprintf("outside file exists or stat failed unexpectedly: %v", err))
		}
	})

	t.Run("zip", func(t *testing.T) {
		require := require.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "symlink-path.zip")
		createZip(t, archivePath, []archiveEntry{{Name: "link/payload", Content: "owned", Mode: 0o644}})
		extractDir := filepath.Join(tmpDir, "extract")
		outsideDir := filepath.Join(tmpDir, "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := os.Symlink(outsideDir, filepath.Join(extractDir, "link")); err != nil {
			require.FailNow(err.Error())
		}
		if err := ExtractZip(archivePath, extractDir); err == nil {
			require.FailNow("expected symlink path error")
		}
		if _, err := os.Stat(filepath.Join(outsideDir, "payload")); !os.IsNotExist(err) {
			require.FailNow(fmt.Sprintf("outside file exists or stat failed unexpectedly: %v", err))
		}
	})
}

func TestExtractTarGzSkipsSymlink(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "symlink.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "evil-link", LinkName: "/etc/passwd", TypeFlag: tar.TypeSymlink},
		{Name: "normal.txt", Content: "ok", Mode: 0o644},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		require.FailNow(t, fmt.Sprintf("ExtractTarGz: %v", err))
	}
	if _, err := os.Stat(filepath.Join(extractDir, "evil-link")); !os.IsNotExist(err) {
		require.FailNow(t, fmt.Sprintf("symlink exists or stat failed unexpectedly: %v", err))
	}
}

func TestExtractTarGzMasksDangerousModeBits(t *testing.T) {
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits not meaningful on Windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "modes.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "tool", Content: "ok", Mode: 0o4755},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		require.FailNow(fmt.Sprintf("ExtractTarGz: %v", err))
	}
	info, err := os.Stat(filepath.Join(extractDir, "tool"))
	if err != nil {
		require.FailNow(err.Error())
	}
	if got := info.Mode(); got&os.ModeSetuid != 0 {
		require.FailNow(fmt.Sprintf("setuid bit preserved: mode=%v", got))
	}
	if got := info.Mode().Perm(); got != 0o755 {
		require.FailNow(fmt.Sprintf("permission bits = %04o, want 0755", got))
	}
}

func TestExtractTarGzExtractsLegacyRegularFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "legacy-regular.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "tool", Content: "legacy", Mode: 0o755, TypeFlag: legacyTarRegularType, TypeFlagSet: true},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		require.FailNow(t, fmt.Sprintf("ExtractTarGz: %v", err))
	}
	got, err := os.ReadFile(filepath.Join(extractDir, "tool"))
	if err != nil {
		require.FailNow(t, err.Error())
	}
	if string(got) != "legacy" {
		require.FailNow(t, fmt.Sprintf("content = %q", got))
	}
}

func TestFetchChecksumFromFileLimitsResponseSize(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", maxChecksumBytes+1)), maxChecksumBytes+1))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	if _, err := c.fetchChecksumFromFile(t.Context(), server.URL, "tool.tar.gz"); err == nil {
		require.FailNow(t, "expected oversized checksum response error")
	}
}

func TestFetchChecksumFromAssetsPropagatesCanceledContext(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "canceled checksum request should not reach server")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	c := Client{BinaryName: "tool"}
	checksum, err := c.fetchChecksumFromAssets(ctx, []*Asset{
		{Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/SHA256SUMS"},
	}, "tool.tar.gz")
	require.Error(err)
	assert.Empty(t, checksum)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFetchChecksumFromAssetsPropagatesOversizedChecksum(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", maxChecksumBytes+1)), maxChecksumBytes+1))
		case "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  tool.tar.gz\n", testHash64)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	checksum, err := c.fetchChecksumFromAssets(t.Context(), []*Asset{
		{Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/SHA256SUMS"},
		{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/checksums.txt"},
	}, "tool.tar.gz")
	require.Error(t, err)
	assert.Empty(t, checksum)
}

func TestFetchChecksumFromAssetsFallsBackAfterMissingAsset(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			http.NotFound(w, r)
		case "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  tool.tar.gz\n", testHash64)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	checksum, err := c.fetchChecksumFromAssets(t.Context(), []*Asset{
		{Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/SHA256SUMS"},
		{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/checksums.txt"},
	}, "tool.tar.gz")
	require.NoError(t, err)
	assert.Equal(t, testHash64, checksum)
}

func TestSanitizeArchivePath(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()
	tests := []struct {
		name string
		path string
		// want is the expected target relative to destDir, in slash form.
		// Empty means the path must be rejected.
		want string
	}{
		{name: "normal", path: "tool", want: "tool"},
		{name: "nested", path: "bin/tool", want: "bin/tool"},
		{name: "dot", path: ".", want: "."},
		{name: "dot slash directory", path: "./", want: "."},
		{name: "inner dot dot staying inside", path: "a/b/../tool", want: "a/tool"},
		{name: "leading dots in name", path: "..foo", want: "..foo"},
		{name: "dots prefix in nested name", path: "a/..b", want: "a/..b"},
		{name: "absolute", path: "/etc/passwd"},
		{name: "absolute root", path: "/"},
		{name: "double slash unc form", path: "//server/share/x"},
		{name: "empty", path: ""},
		{name: "double dot", path: ".."},
		{name: "parent", path: "../x"},
		{name: "deep traversal", path: "../../../etc/passwd"},
		{name: "hidden traversal", path: "a/../../x"},
		{name: "nested hidden traversal", path: "foo/../../../etc/passwd"},
		{name: "trailing parent", path: "a/../.."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertSanitizedArchivePath(t, destDir, tt.path, tt.want)
		})
	}
}

// assertSanitizedArchivePath checks SanitizeArchivePath(destDir, path). An
// empty want means the path must be rejected; otherwise want is the expected
// slash-separated target relative to destDir.
func assertSanitizedArchivePath(t *testing.T, destDir, path, want string) {
	t.Helper()
	got, err := SanitizeArchivePath(destDir, path)
	if want == "" {
		require.Error(t, err, "path %q", path)
		assert.Empty(t, got)
		return
	}
	require.NoError(t, err, "path %q", path)
	assert.Equal(t, filepath.Join(destDir, filepath.FromSlash(want)), got)
}

func TestInstallBinary(t *testing.T) {
	t.Parallel()

	t.Run("sets executable mode", func(t *testing.T) {
		require := require.New(t)
		if runtime.GOOS == "windows" {
			t.Skip("Unix mode bits not meaningful on Windows")
		}
		t.Parallel()
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "src")
		dstPath := filepath.Join(tmpDir, "dst")
		if err := os.WriteFile(srcPath, []byte("binary"), 0o644); err != nil {
			require.FailNow(err.Error())
		}
		if err := InstallBinary(srcPath, dstPath); err != nil {
			require.FailNow(err.Error())
		}
		info, err := os.Stat(dstPath)
		if err != nil {
			require.FailNow(err.Error())
		}
		if got := info.Mode().Perm(); got != 0o755 {
			require.FailNow(fmt.Sprintf("mode = %04o, want 0755", got))
		}
	})

	t.Run("preserves destination on missing source", func(t *testing.T) {
		require := require.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		dstPath := filepath.Join(tmpDir, "tool")
		if err := os.WriteFile(dstPath, []byte("original"), 0o755); err != nil {
			require.FailNow(err.Error())
		}
		err := InstallBinary(filepath.Join(tmpDir, "missing"), dstPath)
		if err == nil {
			require.FailNow("expected missing source error")
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			require.FailNow(err.Error())
		}
		if string(got) != "original" {
			require.FailNow(fmt.Sprintf("content = %q", got))
		}
		if _, err := os.Stat(dstPath + ".new"); !os.IsNotExist(err) {
			require.FailNow(fmt.Sprintf("staging file exists or stat failed unexpectedly: %v", err))
		}
	})

	t.Run("never missing during unix update", func(t *testing.T) {
		require := require.New(t)
		if runtime.GOOS == "windows" {
			t.Skip("Windows moves the running binary aside before replacement")
		}
		t.Parallel()
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "src")
		dstPath := filepath.Join(tmpDir, "tool")
		if err := os.WriteFile(srcPath, []byte("new"), 0o755); err != nil {
			require.FailNow(err.Error())
		}
		if err := os.WriteFile(dstPath, []byte("old"), 0o755); err != nil {
			require.FailNow(err.Error())
		}

		var observations, missing atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := os.Stat(dstPath); os.IsNotExist(err) {
					missing.Add(1)
				}
				observations.Add(1)
			}
		}()

		for i := range 1000 {
			if err := InstallBinary(srcPath, dstPath); err != nil {
				close(stop)
				<-done
				require.FailNow(fmt.Sprintf("iteration %d: %v", i, err))
			}
		}
		close(stop)
		<-done

		if observations.Load() < 1000 {
			t.Skipf("observer ran only %d times", observations.Load())
		}
		if missing.Load() > 0 {
			require.FailNow(fmt.Sprintf("destination missing observations = %d", missing.Load()))
		}
	})
}

func TestExtractChecksum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		assetName string
		want      string
	}{
		{"standard", testHash64 + "  tool_darwin_arm64.tar.gz", "tool_darwin_arm64.tar.gz", testHash64},
		{"uppercase", "ABC123DEF456789012345678901234567890123456789012345678901234ABCD  tool_linux_amd64.tar.gz", "tool_linux_amd64.tar.gz", testHash64},
		{"multiline", fmt.Sprintf("%s  tool_linux_amd64.tar.gz\n%s  tool_darwin_arm64.tar.gz", testHashAAAA, testHashBBBB), "tool_darwin_arm64.tar.gz", testHashBBBB},
		{"no match", testHash64 + "  tool_linux_amd64.tar.gz", "tool_darwin_arm64.tar.gz", ""},
		{"substring filename", testHash64 + "  tool_darwin_arm64.tar.gz.sig", "tool_darwin_arm64.tar.gz", ""},
		{"binary star", testHash64 + " *tool_darwin_arm64.tar.gz", "tool_darwin_arm64.tar.gz", testHash64},
		{"leading dot slash", testHash64 + "  ./tool_darwin_arm64.tar.gz", "tool_darwin_arm64.tar.gz", testHash64},
		{"binary star leading dot slash", testHash64 + " *./tool_darwin_arm64.tar.gz", "tool_darwin_arm64.tar.gz", testHash64},
		{"trailing comment", testHash64 + "  tool_darwin_arm64.tar.gz  # comment", "tool_darwin_arm64.tar.gz", testHash64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractChecksum(tt.body, tt.assetName); got != tt.want {
				require.FailNow(t, fmt.Sprintf("got %q, want %q", got, tt.want))
			}
		})
	}
}

func TestVersionHelpers(t *testing.T) {
	t.Parallel()

	devTests := []struct {
		version string
		want    bool
	}{
		{"dev", true},
		{"unknown", true},
		{"", true},
		{"0.1.0", false},
		{"v0.1.0", false},
		{"0.1.0-2-gabcdef", true},
		{"v0.1.0-2-gabcdef-dirty", true},
		{"0.1.0-rc1", false},
		{"v1.0.0-beta.1", false},
	}
	for _, tt := range devTests {
		t.Run("dev/"+tt.version, func(t *testing.T) {
			t.Parallel()
			if got := IsDevBuildVersion(tt.version); got != tt.want {
				require.FailNow(t, fmt.Sprintf("got %v, want %v", got, tt.want))
			}
		})
	}

	newerTests := []struct {
		name   string
		v1, v2 string
		want   bool
	}{
		{"major", "1.0.0", "0.9.0", true},
		{"same", "1.0.0", "1.0.0", false},
		{"release vs prerelease", "0.4.0", "0.4.0-rc1", true},
		{"prerelease vs release", "0.4.0-rc1", "0.4.0", false},
		{"rc10 vs rc2", "0.4.0-rc10", "0.4.0-rc2", true},
		{"hash", "0.4.2", "88be010", false},
		{"dev base", "0.5.0", "0.4.0-5-gabcdef", true},
	}
	for _, tt := range newerTests {
		t.Run("newer/"+tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNewer(tt.v1, tt.v2); got != tt.want {
				require.FailNow(t, fmt.Sprintf("got %v, want %v", got, tt.want))
			}
		})
	}
}

func TestDefaultAssetNameAndFormatSize(t *testing.T) {
	t.Parallel()

	name := DefaultAssetName(AssetRequest{
		BinaryName: "tool",
		Version:    "1.2.3",
		GOOS:       "linux",
		GOARCH:     "amd64",
		Extension:  ".tar.gz",
	})
	if name != "tool_1.2.3_linux_amd64.tar.gz" {
		require.FailNow(t, fmt.Sprintf("asset name = %q", name))
	}

	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := FormatSize(tt.bytes); got != tt.want {
				require.FailNow(t, fmt.Sprintf("got %q, want %q", got, tt.want))
			}
		})
	}
}

type archiveEntry struct {
	Name        string
	Content     string
	Mode        int64
	TypeFlag    byte
	TypeFlagSet bool
	LinkName    string
}

func createTarGz(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		typeFlag := entry.TypeFlag
		if typeFlag == 0 && !entry.TypeFlagSet {
			typeFlag = tar.TypeReg
		}
		data := []byte(entry.Content)
		header := &tar.Header{
			Name:     entry.Name,
			Mode:     mode,
			Size:     int64(len(data)),
			Typeflag: typeFlag,
			Linkname: entry.LinkName,
		}
		if typeFlag != tar.TypeReg && typeFlag != legacyTarRegularType {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			require.FailNow(t, err.Error())
		}
		if typeFlag == tar.TypeReg || typeFlag == legacyTarRegularType {
			if _, err := tw.Write(data); err != nil {
				require.FailNow(t, err.Error())
			}
		}
	}
}

func createZip(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.Name}
		mode := os.FileMode(entry.Mode)
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			require.FailNow(t, err.Error())
		}
		if _, err := io.Copy(w, bytes.NewBufferString(entry.Content)); err != nil {
			require.FailNow(t, err.Error())
		}
	}
}

func signPayload(t *testing.T, payload []byte) (ed25519.PublicKey, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	return publicKey, ed25519.Sign(privateKey, payload)
}
