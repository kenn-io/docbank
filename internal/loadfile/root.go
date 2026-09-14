package loadfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	defaultMaxPackageFiles   = 1_000_000
	defaultMaxPackageBytes   = int64(50 << 30)
	defaultMaxInventoryBytes = int64(256 << 20)
	maxPackageVolumes        = 64
	inventoryReadBatch       = 256
)

type VolumeRemap struct {
	VolumeName   string
	DeclaredRoot string
	MappedRoot   string
}

type Resolver struct {
	Root        string
	VolumeRoots map[string]string
	MaxFiles    int
	MaxBytes    int64
	root        *os.Root

	mu         sync.Mutex
	inventory  map[string]fs.FileInfo
	caseFold   map[string]int
	remappings map[string]VolumeRemap
	digest     string
	fileCount  int
	byteCount  int64
}

type rootDigestEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

func NewResolver(path string, volumeRoots map[string]string) (*Resolver, error) {
	if len(volumeRoots) > maxPackageVolumes {
		return nil, ErrLoadfileLimit
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve approved package root: %w", err)
	}
	opened, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open approved package root: %w", err)
	}
	r := &Resolver{
		Root: abs, VolumeRoots: make(map[string]string, len(volumeRoots)),
		MaxFiles: defaultMaxPackageFiles, MaxBytes: defaultMaxPackageBytes,
		root: opened, inventory: make(map[string]fs.FileInfo), caseFold: make(map[string]int),
		remappings: make(map[string]VolumeRemap),
	}
	for volume, mapped := range volumeRoots {
		if volume == "" {
			_ = opened.Close()
			return nil, ErrUnsafeReference
		}
		clean, cleanErr := portablePackagePath(mapped)
		if cleanErr != nil {
			_ = opened.Close()
			return nil, cleanErr
		}
		r.VolumeRoots[volume] = clean
	}
	if err := r.buildInventory(); err != nil {
		_ = opened.Close()
		return nil, err
	}
	for _, mapped := range r.VolumeRoots {
		info, ok := r.inventory[mapped]
		if !ok || !info.IsDir() || r.caseFold[strings.ToLower(mapped)] != 1 {
			_ = opened.Close()
			return nil, ErrUnsafeReference
		}
	}
	return r, nil
}

func (r *Resolver) buildInventory() error {
	entries := make([]rootDigestEntry, 0, 128)
	files := 0
	var bytes int64
	var inventoryBytes int64
	err := r.walkInventory(func(name string, info fs.FileInfo) error {
		clean, err := portablePackagePath(name)
		if err != nil {
			return err
		}
		entryBytes := int64(256 + len(clean))
		if entryBytes > defaultMaxInventoryBytes-inventoryBytes {
			return ErrLoadfileLimit
		}
		inventoryBytes += entryBytes
		r.inventory[clean] = info
		r.caseFold[strings.ToLower(clean)]++
		entries = append(entries, rootDigestEntry{Path: clean, Size: info.Size(), Mode: uint32(info.Mode())})
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
			if files > r.MaxFiles || bytes > r.MaxBytes {
				return ErrLoadfileLimit
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inventory approved package root: %w", err)
	}
	slices.SortFunc(entries, func(a, b rootDigestEntry) int { return strings.Compare(a.Path, b.Path) })
	encoded, err := canonical.Marshal(struct {
		Domain  string            `json:"domain"`
		Entries []rootDigestEntry `json:"entries"`
	}{Domain: "package-root/v1", Entries: entries})
	if err != nil {
		return fmt.Errorf("digest approved package root: %w", err)
	}
	digest := sha256.Sum256(encoded)
	r.digest = hex.EncodeToString(digest[:])
	r.fileCount = files
	r.byteCount = bytes
	return nil
}

// walkInventory reads each directory in fixed-size batches. The standard walk
// helpers sort a directory's complete entry list before invoking callbacks.
func (r *Resolver) walkInventory(visit func(string, fs.FileInfo) error) error {
	var walk func(string) error
	walk = func(directory string) error {
		opened, err := r.root.Open(directory)
		if err != nil {
			return ErrUnsafeReference
		}
		defer func() { _ = opened.Close() }()
		if directory != "." {
			expected, statErr := r.root.Lstat(directory)
			actual, openStatErr := opened.Stat()
			if statErr != nil || openStatErr != nil || !expected.IsDir() || !actual.IsDir() || !os.SameFile(expected, actual) {
				return ErrUnsafeReference
			}
		}
		for {
			entries, readErr := opened.ReadDir(inventoryReadBatch)
			for _, entry := range entries {
				name := entry.Name()
				if directory != "." {
					name = directory + "/" + name
				}
				info, statErr := r.root.Lstat(name)
				if statErr != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
					return ErrUnsafeReference
				}
				if err := visit(name, info); err != nil {
					return err
				}
				if info.IsDir() {
					if err := walk(name); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return ErrUnsafeReference
			}
		}
	}
	return walk(".")
}

// DiscoverPackageFiles derives the load-file layout from the cached confined
// inventory without traversing or reopening the caller's tree.
func (r *Resolver) DiscoverPackageFiles() (string, string, []Volume, error) {
	var datFiles, optFiles []string
	volumeNames := make(map[string]bool)
	for name, info := range r.inventory {
		if !info.Mode().IsRegular() {
			continue
		}
		volume, _, found := strings.Cut(name, "/")
		if !found {
			continue
		}
		volumeNames[volume] = true
		if len(volumeNames) > maxPackageVolumes {
			return "", "", nil, ErrLoadfileLimit
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".dat":
			datFiles = append(datFiles, name)
		case ".opt":
			optFiles = append(optFiles, name)
		}
	}
	if len(datFiles) != 1 || len(optFiles) > 1 {
		return "", "", nil, fmt.Errorf("%w: package root must contain exactly one DAT and at most one OPT", ErrMalformedInput)
	}
	slices.Sort(datFiles)
	slices.Sort(optFiles)
	names := make([]string, 0, len(volumeNames))
	for name := range volumeNames {
		names = append(names, name)
	}
	slices.Sort(names)
	volumes := make([]Volume, len(names))
	for index, name := range names {
		volumes[index] = Volume{Name: name, DeclaredRoot: name, Ordinal: index + 1}
	}
	opt := ""
	if len(optFiles) == 1 {
		opt = optFiles[0]
	}
	return datFiles[0], opt, volumes, nil
}

func (r *Resolver) nameFor(volume Volume, relPath string) (string, error) {
	if r.MaxFiles <= 0 || r.MaxBytes <= 0 || r.fileCount > r.MaxFiles || r.byteCount > r.MaxBytes {
		return "", ErrLoadfileLimit
	}
	if volume.Name == "" || volume.DeclaredRoot == "" {
		return "", ErrUnsafeReference
	}
	base := volume.DeclaredRoot
	if mapped, ok := r.VolumeRoots[volume.Name]; ok {
		base = mapped
	}
	cleanBase, err := portablePackagePath(base)
	if err != nil {
		return "", err
	}
	cleanPath, err := portablePackagePath(relPath)
	if err != nil {
		return "", err
	}
	name := cleanBase + "/" + cleanPath
	if r.caseFold[strings.ToLower(name)] != 1 {
		return "", ErrUnsafeReference
	}
	if _, ok := r.inventory[name]; !ok {
		return "", ErrUnsafeReference
	}
	return name, nil
}

func (r *Resolver) Resolve(volume Volume, relPath string) (string, error) {
	name, err := r.nameFor(volume, relPath)
	if err != nil {
		return "", err
	}
	file, err := r.openName(name)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close resolved package file: %w", err)
	}
	r.recordRemapping(volume)
	return filepath.Join(r.Root, filepath.FromSlash(name)), nil
}

func (r *Resolver) Open(volume Volume, relPath string) (*os.File, error) {
	name, err := r.nameFor(volume, relPath)
	if err != nil {
		return nil, err
	}
	file, err := r.openName(name)
	if err != nil {
		return nil, err
	}
	r.recordRemapping(volume)
	return file, nil
}

func (r *Resolver) recordRemapping(volume Volume) {
	mapped, ok := r.VolumeRoots[volume.Name]
	if !ok {
		return
	}
	r.mu.Lock()
	r.remappings[volume.Name] = VolumeRemap{VolumeName: volume.Name, DeclaredRoot: volume.DeclaredRoot, MappedRoot: mapped}
	r.mu.Unlock()
}

func (r *Resolver) openName(name string) (*os.File, error) {
	parts := strings.Split(name, "/")
	current := r.root
	var owned []*os.Root
	defer func() {
		for _, root := range owned {
			_ = root.Close()
		}
	}()
	for index, part := range parts[:len(parts)-1] {
		path := strings.Join(parts[:index+1], "/")
		expected := r.inventory[path]
		if expected == nil || !expected.IsDir() {
			return nil, ErrUnsafeReference
		}
		if info, err := current.Lstat(part); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafeReference
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			return nil, ErrUnsafeReference
		}
		info, err := next.Stat(".")
		if err != nil || !os.SameFile(expected, info) {
			_ = next.Close()
			return nil, ErrUnsafeReference
		}
		owned = append(owned, next)
		current = next
	}
	leaf := parts[len(parts)-1]
	expected := r.inventory[name]
	if expected == nil || !expected.Mode().IsRegular() {
		return nil, ErrUnsafeReference
	}
	before, err := current.Lstat(leaf)
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(expected, before) {
		return nil, ErrUnsafeReference
	}
	file, err := openRootRegular(current, leaf)
	if err != nil {
		return nil, ErrUnsafeReference
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(expected, after) {
		_ = file.Close()
		return nil, ErrUnsafeReference
	}
	return file, nil
}

func (r *Resolver) Close() error { return r.root.Close() }

func (r *Resolver) RootDigest() (string, error) {
	if r.digest == "" {
		return "", errorsNewRootDigest()
	}
	return r.digest, nil
}

func errorsNewRootDigest() error { return errors.New("approved package root digest is unavailable") }

func (r *Resolver) Remappings() []VolumeRemap {
	r.mu.Lock()
	defer r.mu.Unlock()
	remappings := make([]VolumeRemap, 0, len(r.remappings))
	for _, remapping := range r.remappings {
		remappings = append(remappings, remapping)
	}
	slices.SortFunc(remappings, func(a, b VolumeRemap) int { return strings.Compare(a.VolumeName, b.VolumeName) })
	return remappings
}
