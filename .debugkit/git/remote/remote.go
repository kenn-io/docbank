// Package gitremote validates forge repository identities, clone paths, and
// remote URLs.
//
// The helpers are intentionally conservative around filesystem paths: host,
// owner, and repository names are treated as path components only after
// rejecting absolute paths, traversal, empty path segments, and backslashes.
// Remote URL validation accepts local paths for test fixtures, but verifies
// host and owner/name when a remote URL exposes those pieces.
package gitremote

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// Identity identifies a repository on a forge host.
type Identity struct {
	// Host is the forge hostname, for example "github.com".
	Host string
	// Owner is the repository owner or namespace. Slash-separated group paths
	// are allowed for forges such as GitLab.
	Owner string
	// Name is the repository name without a ".git" suffix.
	Name string
}

// ClonePath returns {baseDir}/{host}/{owner}/{name}.git after validating every
// segment against absolute paths, traversal, empty path parts, and backslashes.
func ClonePath(baseDir string, id Identity) (string, error) {
	if err := validatePathValue("host", id.Host, false, false); err != nil {
		return "", err
	}
	if err := validatePathValue("owner", id.Owner, true, false); err != nil {
		return "", err
	}
	if err := validatePathValue("name", id.Name, false, false); err != nil {
		return "", err
	}
	clonePath := filepath.Join(baseDir, id.Host, id.Owner, id.Name+".git")
	rel, err := relativeClonePath(baseDir, clonePath)
	if err != nil {
		return "", err
	}
	if err := validatePathValue("relative", rel, true, false); err != nil {
		return "", err
	}
	return clonePath, nil
}

func relativeClonePath(baseDir, clonePath string) (string, error) {
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve clone base: %w", err)
	}
	cloneAbs, err := filepath.Abs(clonePath)
	if err != nil {
		return "", fmt.Errorf("resolve clone path: %w", err)
	}
	rel, err := filepath.Rel(baseAbs, cloneAbs)
	if err != nil {
		return "", fmt.Errorf("resolve clone relative path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

func validatePathValue(label, value string, allowSlash, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if value == "" || strings.TrimSpace(value) != value || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return fmt.Errorf("unsafe clone path %s %q", label, value)
	}
	if !allowSlash && strings.Contains(value, "/") {
		return fmt.Errorf("unsafe clone path %s %q", label, value)
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("unsafe clone path %s %q", label, value)
		}
	}
	return nil
}

// ValidateRemoteIdentity verifies that remoteURL belongs to id when the remote
// spelling exposes a host and repository path. Local paths and file:// remotes
// are accepted because test fixtures often use them.
func ValidateRemoteIdentity(id Identity, remoteURL string) error {
	if err := ValidateRemoteHost(id.Host, remoteURL); err != nil {
		return err
	}
	actualRepo := RemoteRepoPath(remoteURL)
	if actualRepo == "" {
		return nil
	}
	expectedRepo := strings.Trim(strings.TrimSpace(id.Owner)+"/"+strings.TrimSpace(id.Name), "/")
	if !strings.EqualFold(actualRepo, expectedRepo) {
		return fmt.Errorf("clone remote repo %q does not match configured repo %q", actualRepo, expectedRepo)
	}
	return nil
}

// ValidateRemoteHost verifies that remoteURL belongs to expectedHost when the
// remote spelling exposes a host. Local paths and file:// remotes are accepted.
func ValidateRemoteHost(expectedHost, remoteURL string) error {
	actualHost := RemoteHost(remoteURL)
	if actualHost == "" {
		return nil
	}
	if NormalizeHost(actualHost) != NormalizeHost(expectedHost) {
		return fmt.Errorf("clone remote host %q does not match configured platform host %q", actualHost, expectedHost)
	}
	return nil
}

// RemoteHost extracts the host from HTTPS/SSH URLs and SCP-like remotes.
func RemoteHost(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" || IsLocal(remoteURL) {
		return ""
	}
	if u, err := url.Parse(remoteURL); err == nil && u.Host != "" {
		return u.Host
	}
	prefix, _, ok := strings.Cut(remoteURL, ":")
	if !ok || strings.Contains(prefix, "/") {
		return ""
	}
	if at := strings.LastIndex(prefix, "@"); at >= 0 {
		prefix = prefix[at+1:]
	}
	return prefix
}

// RemoteRepoPath extracts owner/name from HTTPS/SSH URLs and SCP-like remotes.
func RemoteRepoPath(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" || IsLocal(remoteURL) {
		return ""
	}
	var repoPath string
	if u, err := url.Parse(remoteURL); err == nil && u.Host != "" {
		repoPath = u.Path
	} else {
		prefix, path, ok := strings.Cut(remoteURL, ":")
		if !ok || strings.Contains(prefix, "/") {
			return ""
		}
		repoPath = path
	}
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	if repoPath == "" || strings.Contains(repoPath, "\\") {
		return ""
	}
	return repoPath
}

// CloneURLIdentity normalizes a hosted clone URL to "host/owner/name",
// preserving repository-path case. Local paths and unrecognized spellings
// are returned trimmed.
func CloneURLIdentity(remoteURL string) string {
	trimmed := strings.TrimSpace(remoteURL)
	if trimmed == "" {
		return ""
	}
	host := cloneURLHost(trimmed)
	repoPath := RemoteRepoPath(trimmed)
	if host == "" || repoPath == "" {
		return trimmed
	}
	return host + "/" + repoPath
}

func cloneURLHost(remoteURL string) string {
	if parsed, err := url.Parse(remoteURL); err == nil && parsed.Host != "" {
		host := strings.ToLower(parsed.Hostname())
		port := parsed.Port()
		if port == "" || port == defaultPort(parsed.Scheme) {
			return host
		}
		return net.JoinHostPort(host, port)
	}
	return NormalizeHost(RemoteHost(remoteURL))
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	case "ssh", "git+ssh", "ssh+git":
		return "22"
	case "git":
		return "9418"
	default:
		return ""
	}
}

// IsLocal reports whether remoteURL is a local filesystem remote.
func IsLocal(remoteURL string) bool {
	if filepath.VolumeName(remoteURL) != "" || isWindowsDrivePath(remoteURL) {
		return true
	}
	if u, err := url.Parse(remoteURL); err == nil && strings.EqualFold(u.Scheme, "file") {
		return true
	}
	return false
}

func isWindowsDrivePath(value string) bool {
	if len(value) < 3 || value[1] != ':' {
		return false
	}
	drive := value[0]
	if (drive < 'A' || drive > 'Z') && (drive < 'a' || drive > 'z') {
		return false
	}
	return value[2] == '\\' || value[2] == '/'
}

// NormalizeHost lowercases host, trims IPv6 brackets, and removes :443.
func NormalizeHost(host string) string {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if before, ok := strings.CutSuffix(host, ":443"); ok {
		return before
	}
	return host
}
