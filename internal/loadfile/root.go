package loadfile

import (
	"context"
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

	"go.kenn.io/docbank/internal/canonical"
)

const (
	defaultMaxPackageFiles   = 1_000_000
	defaultMaxPackageBytes   = int64(50 << 30)
	defaultMaxInventoryBytes = int64(256 << 20)
	maxPackageVolumes        = 64
	inventoryReadBatch       = 256
)

type Resolver struct {
	Root        string
	VolumeRoots map[string]string
	MaxFiles    int
	MaxBytes    int64
	root        *os.Root

	inventory map[string]fs.FileInfo
	caseFold  map[string]int
	digest    string
	fileCount int
	byteCount int64
}

type rootDigestEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

func NewResolver(ctx context.Context, path string, volumeRoots map[string]string) (*Resolver, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: package source path is required", ErrUnsafeReference)
	}
	if len(volumeRoots) > maxPackageVolumes {
		return nil, ErrLoadfileLimit
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot resolve package directory", ErrUnsafeReference)
	}
	opened, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open package directory", ErrUnsafeReference)
	}
	r := &Resolver{
		Root: abs, VolumeRoots: make(map[string]string, len(volumeRoots)),
		MaxFiles: defaultMaxPackageFiles, MaxBytes: defaultMaxPackageBytes,
		root: opened, inventory: make(map[string]fs.FileInfo), caseFold: make(map[string]int),
	}
	if err := r.buildInventory(ctx); err != nil {
		_ = opened.Close()
		return nil, err
	}
	if err := r.SetVolumeRoots(volumeRoots); err != nil {
		_ = opened.Close()
		return nil, err
	}
	return r, nil
}

// SetVolumeRoots validates a confirmed mapping against the existing inventory.
func (r *Resolver) SetVolumeRoots(volumeRoots map[string]string) error {
	if len(volumeRoots) > maxPackageVolumes {
		return ErrLoadfileLimit
	}
	roots := make(map[string]string, len(volumeRoots))
	for volume, mapped := range volumeRoots {
		if volume == "" {
			return ErrUnsafeReference
		}
		clean, err := portablePackagePath(mapped)
		if err != nil {
			return err
		}
		info, ok := r.inventory[clean]
		if !ok || !info.IsDir() || r.caseFold[strings.ToLower(clean)] != 1 {
			return ErrUnsafeReference
		}
		roots[volume] = clean
	}
	r.VolumeRoots = roots
	return nil
}

// EnforceMaxFileBytes rejects a package before parsing or hashing when any
// inventoried object is too large for the destination's ingest path.
func (r *Resolver) EnforceMaxFileBytes(maxBytes int64) error {
	if maxBytes <= 0 {
		return ErrLoadfileLimit
	}
	for _, info := range r.inventory {
		if info.Mode().IsRegular() && info.Size() > maxBytes {
			return ErrLoadfileLimit
		}
	}
	return nil
}

func (r *Resolver) buildInventory(ctx context.Context) error {
	entries := make([]rootDigestEntry, 0, 128)
	files := 0
	var bytes int64
	var inventoryBytes int64
	err := r.walkInventory(ctx, func(name string, info fs.FileInfo) error {
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
func (r *Resolver) walkInventory(ctx context.Context, visit func(string, fs.FileInfo) error) error {
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
				if err := ctx.Err(); err != nil {
					return err
				}
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
			switch strings.ToLower(path.Ext(name)) {
			case ".dat", ".opt", ".lfp":
				return "", "", nil, fmt.Errorf("%w: load files must be inside a volume directory, not directly in the package root", ErrMalformedInput)
			}
			continue
		}
		volumeNames[volume] = true
		if len(volumeNames) > maxPackageVolumes {
			return "", "", nil, ErrLoadfileLimit
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".dat":
			datFiles = append(datFiles, name)
		case ".opt", ".lfp":
			optFiles = append(optFiles, name)
		}
	}
	if len(datFiles) != 1 || len(optFiles) > 1 {
		return "", "", nil, fmt.Errorf("%w: package root must contain exactly one DAT and at most one OPT or LFP page map", ErrMalformedInput)
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

// Open confines reads and seeks to the inventoried file size. Close must be
// checked: it rejects a size change that occurred while the file was open.
func (r *Resolver) Open(volume Volume, relPath string) (*PackageFile, error) {
	name, err := r.nameFor(volume, relPath)
	if err != nil {
		return nil, err
	}
	file, err := r.openName(name)
	if err != nil {
		return nil, err
	}
	return &PackageFile{SectionReader: io.NewSectionReader(file, 0, r.inventory[name].Size()), file: file}, nil
}

// PackageFile reads an inventoried extent and checks for size changes on close.
type PackageFile struct {
	*io.SectionReader

	file *os.File
}

func (f *PackageFile) Close() error {
	info, err := f.file.Stat()
	if err == nil && info.Size() != f.Size() {
		err = fmt.Errorf("%w: package file size changed during preflight", ErrMalformedInput)
	}
	return errors.Join(err, f.file.Close())
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
	if after.Size() != expected.Size() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: package file size changed during preflight", ErrMalformedInput)
	}
	return file, nil
}

func (r *Resolver) Close() error { return r.root.Close() }

func (r *Resolver) RootDigest() (string, error) {
	if r.digest == "" {
		return "", errors.New("approved package root digest is unavailable")
	}
	return r.digest, nil
}
