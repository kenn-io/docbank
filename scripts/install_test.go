package scripts

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type installerFixture struct {
	mu        sync.RWMutex
	archive   []byte
	checksums string
}

func (f *installerFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch path.Base(r.URL.Path) {
	case "SHA256SUMS":
		_, _ = fmt.Fprint(w, f.checksums)
	default:
		if strings.HasSuffix(r.URL.Path, ".tar.gz") || strings.HasSuffix(r.URL.Path, ".zip") {
			_, _ = w.Write(f.archive)
			return
		}
		http.NotFound(w, r)
	}
}

func (f *installerFixture) set(archive []byte, checksums string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archive = archive
	f.checksums = checksums
}

func TestInstallerVerifiesBeforeReplacingBinary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no installer for this platform")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("no installer for this architecture")
	}

	const version = "9.9.9"
	extension := ".tar.gz"
	binaryName := "docbank"
	if runtime.GOOS == "windows" {
		extension = ".zip"
		binaryName += ".exe"
	}
	archiveName := fmt.Sprintf("docbank_%s_%s_%s%s", version, runtime.GOOS, runtime.GOARCH, extension)
	installDir := t.TempDir()
	destination := filepath.Join(installDir, binaryName)

	fixture := &installerFixture{}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)

	goodArchive := installerArchive(t, binaryName, []byte("new docbank\n"), false)
	goodHash := fmt.Sprintf("%x", sha256.Sum256(goodArchive))
	fixture.set(goodArchive, fmt.Sprintf("%s  %s\n", goodHash, archiveName))
	writeExisting(t, destination)
	output, err := runInstaller(t, server.URL, installDir)
	if err != nil {
		t.Fatalf("installing verified fixture: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Checksum verified") {
		t.Fatalf("installer did not report checksum verification:\n%s", output)
	}
	assertContent(t, destination, "new docbank\n")

	tests := []struct {
		name      string
		archive   []byte
		checksums string
		wantError string
	}{
		{
			name:      "checksum mismatch",
			archive:   goodArchive,
			checksums: fmt.Sprintf("%064d  %s\n", 0, archiveName),
			wantError: "checksum mismatch",
		},
		{
			name:      "duplicate checksum",
			archive:   goodArchive,
			checksums: fmt.Sprintf("%s  %s\n%s  %s\n", goodHash, archiveName, goodHash, archiveName),
			wantError: "exactly one entry",
		},
		{
			name:      "unexpected archive entry",
			archive:   installerArchive(t, binaryName, []byte("new docbank\n"), true),
			wantError: "only",
		},
	}
	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			test := tests[i]
			if test.checksums == "" {
				hash := fmt.Sprintf("%x", sha256.Sum256(test.archive))
				test.checksums = fmt.Sprintf("%s  %s\n", hash, archiveName)
			}
			fixture.set(test.archive, test.checksums)
			writeExisting(t, destination)
			output, err := runInstaller(t, server.URL, installDir)
			if err == nil {
				t.Fatalf("installer accepted invalid release:\n%s", output)
			}
			if !strings.Contains(strings.ToLower(output), strings.ToLower(test.wantError)) {
				t.Fatalf("error does not explain %q:\n%s", test.wantError, output)
			}
			assertContent(t, destination, "old docbank\n")
		})
	}
}

func runInstaller(t *testing.T, baseURL, installDir string) (string, error) {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1")
	} else {
		cmd = exec.Command("sh", "install.sh")
	}
	cmd.Env = append(os.Environ(),
		"DOCBANK_VERSION=v9.9.9",
		"DOCBANK_RELEASE_BASE_URL="+baseURL,
		"DOCBANK_INSTALL_DIR="+installDir,
		"DOCBANK_NO_MODIFY_PATH=1",
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func writeExisting(t *testing.T, destination string) {
	t.Helper()
	if err := os.WriteFile(destination, []byte("old docbank\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, destination, want string) {
	t.Helper()
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("installed content = %q, want %q", got, want)
	}
}

func installerArchive(t *testing.T, binaryName string, content []byte, extra bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		writeZipEntry(t, zw, binaryName, content)
		if extra {
			writeZipEntry(t, zw, "unexpected.txt", []byte("unexpected\n"))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	writeTarEntry(t, tw, binaryName, content)
	if extra {
		writeTarEntry(t, tw, "unexpected.txt", []byte("unexpected\n"))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeZipEntry(t *testing.T, zw *zip.Writer, name string, content []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, content []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
}
