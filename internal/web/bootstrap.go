package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/safefileio"
)

const launchDirName = "web-launch"

// WriteBootstrap writes the browser-session handoff beneath the owner-private
// vault root and returns a credential-free file URL suitable for an OS browser
// launcher. The handoff carries only a daemon-lifetime, read-only token; the
// vault API key never enters this file.
func WriteBootstrap(root, authenticatedURL string) (string, error) {
	if root == "" {
		return "", errors.New("web bootstrap root is empty")
	}
	target, err := url.Parse(authenticatedURL)
	if err != nil || target.Scheme != "http" || target.Host == "" {
		return "", errors.New("web bootstrap requires an authenticated HTTP URL")
	}
	host := target.Hostname()
	ip := net.ParseIP(host)
	localName := strings.EqualFold(host, "localhost") ||
		strings.HasSuffix(strings.ToLower(host), ".localhost")
	values, queryErr := url.ParseQuery(target.Fragment)
	if queryErr != nil || values.Get("web_session") == "" ||
		(!localName && (ip == nil || !ip.IsLoopback())) {
		return "", errors.New("web bootstrap requires an authenticated loopback URL")
	}
	dir := filepath.Join(root, launchDirName)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("securing web bootstrap directory: %w", err)
	}
	path := filepath.Join(dir, "index.html")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("removing previous web bootstrap: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating web bootstrap: %w", err)
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if err := restrictBootstrapFile(path); err != nil {
		return "", err
	}
	encodedURL, err := json.Marshal(authenticatedURL)
	if err != nil {
		return "", fmt.Errorf("encoding web bootstrap destination: %w", err)
	}
	content := []byte(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="referrer" content="no-referrer">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'">
  <title>Opening Docbank</title>
</head>
<body>
  <p>Opening Docbank…</p>
  <script>location.replace(` + string(encodedURL) + `);</script>
</body>
</html>
`)
	if _, err := file.Write(content); err != nil {
		return "", fmt.Errorf("writing web bootstrap: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("syncing web bootstrap: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing web bootstrap: %w", err)
	}
	success = true
	return fileURL(path), nil
}

// RemoveBootstrap removes the credential-bearing handoff when its daemon
// starts or stops. Missing runtime state is already clean.
func RemoveBootstrap(root string) error {
	if root == "" {
		return errors.New("web bootstrap root is empty")
	}
	if err := os.RemoveAll(filepath.Join(root, launchDirName)); err != nil {
		return fmt.Errorf("removing web bootstrap: %w", err)
	}
	return nil
}

func fileURL(path string) string {
	slashPath := filepath.ToSlash(path)
	if volume := filepath.VolumeName(path); volume != "" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}
