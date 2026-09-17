package renderpdf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

// RuntimeFile and RuntimeSymlink are the content-addressed private-root entries.
type RuntimeFile = sandbox.RuntimeFile
type RuntimeSymlink = sandbox.RuntimeSymlink

// RuntimeManifest is the immutable runtime snapshot selected by the operator.
type RuntimeManifest struct {
	Files    []RuntimeFile
	Symlinks []RuntimeSymlink
	Identity string
}

// DefaultRuntimeRoots returns the measured Debian/Ubuntu LibreOffice roots.
func DefaultRuntimeRoots() []string {
	return defaultRuntimeRootsForArch(runtime.GOARCH)
}

func defaultRuntimeRootsForArch(goarch string) []string {
	// ponytail: Debian/Ubuntu discovery roots, upgrade trigger: measured non-Debian runtime manifest.
	roots := []string{
		"/usr/lib/libreoffice", "/usr/share/libreoffice", "/etc/libreoffice",
		"/var/lib/libreoffice",
		"/usr/share/fonts", "/usr/share/fontconfig", "/var/cache/fontconfig",
		"/etc/fonts", "/usr/lib/locale", "/etc/ld.so.cache", "/etc/localtime",
	}
	switch goarch {
	case "amd64":
		roots = append(roots,
			"/usr/lib/x86_64-linux-gnu",
			"/lib/x86_64-linux-gnu",
			"/usr/lib64",
			"/lib64/ld-linux-x86-64.so.2",
		)
	case "arm64":
		roots = append(roots,
			"/usr/lib/aarch64-linux-gnu",
			"/lib/aarch64-linux-gnu",
			"/lib/ld-linux-aarch64.so.1",
		)
	}
	return roots
}

// DiscoverRuntime snapshots declared regular files and explicit safe symlinks.
func DiscoverRuntime(roots []string) (RuntimeManifest, error) {
	manifest := RuntimeManifest{}
	seen := make(map[string]struct{})
	var total int64
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return RuntimeManifest{}, errors.New("runtime root must be absolute and clean")
		}
		if err := discoverRuntimePath(root, root, true, &manifest, seen, &total); err != nil {
			return RuntimeManifest{}, err
		}
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].GuestPath < manifest.Files[j].GuestPath
	})
	sort.Slice(manifest.Symlinks, func(i, j int) bool {
		return manifest.Symlinks[i].GuestPath < manifest.Symlinks[j].GuestPath
	})
	if len(manifest.Files)+len(manifest.Symlinks) > sandbox.MaxRuntimeEntries {
		return RuntimeManifest{}, errors.New("runtime entry count exceeds limit")
	}
	identity, err := runtimeIdentityForManifest(manifest.Files, manifest.Symlinks)
	if err != nil {
		return RuntimeManifest{}, err
	}
	manifest.Identity = identity
	return manifest, nil
}

func discoverRuntimePath(path, guest string, selectedRoot bool, manifest *RuntimeManifest, seen map[string]struct{}, total *int64) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if runtimeDepth(guest) > sandbox.MaxRuntimeDepth {
		return errors.New("runtime path depth exceeds limit")
	}
	switch mode := info.Mode() & os.ModeType; mode {
	case 0:
		return addRuntimeFile(path, guest, info, manifest, seen, total)
	case os.ModeDir:
		return filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(path, current)
			if err != nil {
				return err
			}
			target := guest
			if relative != "." {
				target = filepath.Join(guest, relative)
			}
			if runtimeDepth(target) > sandbox.MaxRuntimeDepth {
				return errors.New("runtime path depth exceeds limit")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return discoverRuntimeSymlink(current, target, false, manifest, seen, total)
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type() != 0 {
				return sandbox.ErrRuntimeSpecialFile
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspect runtime entry %s: %w", current, err)
			}
			return addRuntimeFile(current, target, info, manifest, seen, total)
		})
	case os.ModeSymlink:
		return discoverRuntimeSymlink(path, guest, selectedRoot, manifest, seen, total)
	default:
		return sandbox.ErrRuntimeSpecialFile
	}
}

func discoverRuntimeSymlink(path, guest string, selectedRoot bool, manifest *RuntimeManifest, seen map[string]struct{}, total *int64) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return discoverRuntimePath(resolved, guest, false, manifest, seen, total)
	}
	if !selectedRoot && target != "" && !filepath.IsAbs(target) &&
		filepath.Clean(target) == target && !strings.Contains(target, "..") {
		if err := addRuntimeSymlink(guest, target, manifest, seen); err == nil {
			return nil
		}
	}
	if !info.Mode().IsRegular() {
		return sandbox.ErrRuntimeSpecialFile
	}
	return addRuntimeFile(resolved, guest, info, manifest, seen, total)
}

func addRuntimeSymlink(guest, target string, manifest *RuntimeManifest, seen map[string]struct{}) error {
	if _, found := seen[guest]; found {
		return errors.New("runtime guest path is duplicated")
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target ||
		strings.ContainsAny(target, "\x00") || strings.Contains(target, "..") {
		return errors.New("runtime symlink target is invalid")
	}
	seen[guest] = struct{}{}
	manifest.Symlinks = append(manifest.Symlinks, RuntimeSymlink{GuestPath: guest, Target: target})
	return nil
}

func addRuntimeFile(path, guest string, info os.FileInfo, manifest *RuntimeManifest, seen map[string]struct{}, total *int64) error {
	if _, found := seen[guest]; found {
		return errors.New("runtime guest path is duplicated")
	}
	if !info.Mode().IsRegular() {
		return sandbox.ErrRuntimeSpecialFile
	}
	if info.Size() < 0 || info.Size() > sandbox.MaxRuntimeBytes-*total {
		return errors.New("runtime byte bound exceeded")
	}
	hash, executable, err := hashRuntimeFile(path, info)
	if err != nil {
		return err
	}
	seen[guest] = struct{}{}
	manifest.Files = append(manifest.Files, RuntimeFile{
		SourcePath: path, GuestPath: guest, SHA256: hash, Executable: executable,
	})
	*total += info.Size()
	return nil
}

func hashRuntimeFile(path string, info os.FileInfo) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 4)
	if _, err := io.ReadFull(file, head); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", false, err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", false, err
	}
	executable := info.Mode()&0o111 != 0 || string(head) == "\x7fELF"
	return hex.EncodeToString(sum.Sum(nil)), executable, nil
}

func runtimeDepth(path string) int {
	trimmed := strings.Trim(path, string(filepath.Separator))
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, string(filepath.Separator))
}

func runtimeIdentityForManifest(files []RuntimeFile, symlinks []RuntimeSymlink) (string, error) {
	type identityFile struct {
		GuestPath  string `json:"guest_path"`
		SHA256     string `json:"sha256"`
		Bytes      int64  `json:"bytes"`
		Executable bool   `json:"executable"`
	}
	type identityManifest struct {
		Files    []identityFile   `json:"files"`
		Symlinks []RuntimeSymlink `json:"symlinks"`
	}
	value := identityManifest{Symlinks: append([]RuntimeSymlink(nil), symlinks...)}
	value.Files = make([]identityFile, 0, len(files))
	var total int64
	for _, file := range files {
		info, err := os.Stat(file.SourcePath)
		if err != nil || !info.Mode().IsRegular() {
			return "", sandbox.ErrRuntimeSpecialFile
		}
		total += info.Size()
		if total > sandbox.MaxRuntimeBytes {
			return "", errors.New("runtime byte bound exceeded")
		}
		value.Files = append(value.Files, identityFile{
			GuestPath: file.GuestPath, SHA256: file.SHA256,
			Bytes: info.Size(), Executable: file.Executable,
		})
	}
	sort.Slice(value.Files, func(i, j int) bool { return value.Files[i].GuestPath < value.Files[j].GuestPath })
	sort.Slice(value.Symlinks, func(i, j int) bool { return value.Symlinks[i].GuestPath < value.Symlinks[j].GuestPath })
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
