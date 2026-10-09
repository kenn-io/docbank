package main

import (
	"bytes"
	"encoding/json/v2"
	"image/color"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestWildcardDaemonKeepsAuthenticatedLocalDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_BIND_ADDR", "0.0.0.0")
	t.Setenv("DOCBANK_API_PORT", "0")
	t.Setenv("DOCBANK_API_KEY", "synthetic-network-key")
	t.Setenv("DOCBANK_API_KEY_FILE", "")
	t.Setenv("DOCBANK_ALLOWED_HOSTS", "docbank")
	startServe(t)
	rec := waitForDaemon(t, root)
	host, _, err := net.SplitHostPort(rec.Address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	assert.True(t, ip.IsLoopback(), host)
	client, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	info, err := client.API().VaultInfo(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, info.VaultID)
	for _, test := range []struct {
		host, key string
		status    int
	}{
		{"docbank", "synthetic-network-key", 200}, {"docbank", "", 401}, {"unconfigured.example", "synthetic-network-key", 403},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+rec.Address+"/api/v1/info", nil)
		require.NoError(t, err)
		request.Host = test.host
		request.Header.Set("Authorization", "Bearer "+test.key)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Equal(t, test.status, response.StatusCode, test.host)
	}
}

func TestLocalDiscoveryReusesLoopbackAndWildcardListeners(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		network, address, localHost string
	}{
		{"tcp4", "127.0.0.1:0", "127.0.0.1"},
		{"tcp4", "0.0.0.0:0", "127.0.0.1"},
		{"tcp6", "[::1]:0", "::1"},
		{"tcp6", "[::]:0", "::1"},
	} {
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()
			listener, err := net.Listen(test.network, test.address)
			if err != nil && test.network == "tcp6" {
				t.Skipf("IPv6 unavailable: %v", err)
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			local, address, err := listenLocalDiscovery(t.Context(), listener)
			require.NoError(t, err)
			assert.Nil(t, local)
			host, port, err := net.SplitHostPort(address)
			require.NoError(t, err)
			_, boundPort, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(t, err)
			assert.Equal(t, test.localHost, host)
			assert.Equal(t, boundPort, port)
			connection, err := net.DialTimeout(test.network, address, time.Second)
			require.NoError(t, err)
			require.NoError(t, connection.Close())
		})
	}
}

func TestConcreteNetworkDaemonKeepsLocalImports(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	require.NoError(t, err)
	var bindIP string
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsGlobalUnicast() {
			bindIP = ip.String()
			break
		}
	}
	if bindIP == "" {
		t.Skip("no concrete non-loopback IPv4 interface")
	}
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_BIND_ADDR", bindIP)
	t.Setenv("DOCBANK_API_PORT", "0")
	t.Setenv("DOCBANK_API_KEY", "synthetic-network-key")
	t.Setenv("DOCBANK_API_KEY_FILE", "")
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "0")
	t.Setenv("DOCBANK_ALLOWED_HOSTS", "docbank")
	stop := startServe(t)
	rec := waitForDaemon(t, root)
	networkAddress := rec.Metadata["network_address"]
	networkHost, networkPort, err := net.SplitHostPort(networkAddress)
	require.NoError(t, err)
	require.Equal(t, bindIP, networkHost)
	require.NotEqual(t, "0", networkPort)
	host, _, err := net.SplitHostPort(rec.Address)
	require.NoError(t, err)
	require.True(t, net.ParseIP(host).IsLoopback(), rec.Address)
	require.NotEqual(t, networkAddress, rec.Address)
	client, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	_, err = client.API().VaultInfo(t.Context())
	require.NoError(t, err)

	source := writeSourceFile(t, "synthetic-note.txt", "synthetic note")
	out, err := runCLI(t, "add", source, "--preflight", "--json")
	require.NoError(t, err, out)
	var preflight api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(out), &preflight))
	assert.Zero(t, preflight.Errors)
	for _, args := range [][]string{
		{"add", source, "--dest", "/json-import", "--json"},
		{"add", source, "--dest", "/stream-import", "--progress", "plain"},
	} {
		out, err = runCLI(t, args...)
		require.NoError(t, err, out)
	}

	// Use the advertised endpoint with the same key for photo imports and
	// directory package preflight, the other server-path import routes.
	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	t.Cleanup(httpClient.CloseIdleConnections)
	post := func(address, path string, body any) (int, []byte) {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+path, bytes.NewReader(data))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", rec.Metadata["api_key"])
		request.Header.Set("X-Forwarded-For", "127.0.0.1")
		response, err := httpClient.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		data, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, data
	}
	photos := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(photos, "capture.jpg"), mediatest.JPEG(2, 2, color.White), 0o600))
	photoRequest := api.PhotoImportStartRequest{SourceRoot: photos, Destination: "/photos"}
	status, body := post(rec.Address, "/api/v1/photos/imports", photoRequest)
	require.Equal(t, http.StatusAccepted, status, string(body))
	var operation api.StorageOperation
	require.NoError(t, json.Unmarshal(body, &operation))
	require.NotEmpty(t, operation.ID)
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+rec.Address+"/api/v1/jobs/"+operation.ID, nil)
		if err != nil {
			return false
		}
		request.Header.Set("X-Api-Key", rec.Metadata["api_key"])
		response, err := httpClient.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		var latest api.StorageOperation
		return json.UnmarshalRead(response.Body, &latest) == nil && latest.State == "completed" && latest.CompletedObjects == 1
	}, 30*time.Second, 50*time.Millisecond)

	packageRoot := t.TempDir()
	volumeRoot := filepath.Join(packageRoot, "VOL001")
	require.NoError(t, os.Mkdir(volumeRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(volumeRoot, "records.csv"), []byte("DOCID,NATIVE\nDOC-A,note.txt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(volumeRoot, "note.txt"), []byte("synthetic package note"), 0o600))
	packageRequest := api.PackagePreflightRequest{Profile: "csv-rfc4180-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: packageRoot}
	status, body = post(rec.Address, "/api/v1/packages/preflights", packageRequest)
	require.Equal(t, http.StatusOK, status, string(body))
	var packagePreflight api.PackagePreflight
	require.NoError(t, json.Unmarshal(body, &packagePreflight))
	assert.Equal(t, 1, packagePreflight.Records)
	assert.NotEmpty(t, packagePreflight.PreflightID)
	assert.False(t, packagePreflight.Blocking)

	for _, test := range []struct {
		path string
		body any
	}{
		{"/api/v1/ingest", api.IngestRequest{Paths: []string{source}, Dest: "/remote"}},
		{"/api/v1/ingest/stream", api.IngestRequest{Paths: []string{source}, Dest: "/remote"}},
		{"/api/v1/ingest/preflight", api.IngestPreflightRequest{Paths: []string{source}}},
		{"/api/v1/photos/imports", photoRequest},
		{"/api/v1/packages/preflights", packageRequest},
	} {
		status, body := post(networkAddress, test.path, test.body)
		assert.Equal(t, http.StatusForbidden, status, test.path+": "+string(body))
		assert.Contains(t, string(body), "loopback_only")
	}
	stop()
	for _, address := range []string{rec.Address, networkAddress} {
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err, "listener must be released on shutdown: %s", address)
		require.NoError(t, listener.Close())
	}
	records, err := daemonconn.RuntimeStore(root).List()
	require.NoError(t, err)
	assert.Empty(t, records)
}
