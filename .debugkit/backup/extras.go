package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExtrasDirSpec walks one DataDir-relative directory recursively; every
// regular file found is recorded under its DataDir-relative path. A missing
// directory is skipped, not an error: operational directories often appear
// only once the application has something to put in them.
type ExtrasDirSpec struct {
	Name string
	// Sensitive marks the directory as carrying secrets (see ExtrasSpec).
	Sensitive bool
}

// ExtrasGlobSpec matches file basenames at DataDir's top level. The pattern
// must be a pure basename (no separators); matching happens against directory
// entries, so a DataDir path containing glob metacharacters cannot corrupt it.
type ExtrasGlobSpec struct {
	Pattern   string
	Sensitive bool
}

// ExtrasFileSpec captures one file, which may live outside DataDir, recorded
// in the tree under RecordAs (a slash-separated relative path). Unlike dir
// walks, a missing file is an error: naming a specific file is an explicit
// request that must not silently produce a snapshot without it.
type ExtrasFileSpec struct {
	Path      string
	RecordAs  string
	Sensitive bool
}

// ExtrasSpec declares which operational files ride along with a snapshot.
// The engine imposes no default set: the application decides what its
// snapshots carry. Sources marked Sensitive are refused on an unencrypted
// repository unless the caller sets AllowPlaintextSecrets, so secrets never
// land in plaintext packs by accident.
type ExtrasSpec struct {
	Dirs  []ExtrasDirSpec
	Globs []ExtrasGlobSpec
	Files []ExtrasFileSpec
}

// empty reports whether the spec selects nothing.
func (s ExtrasSpec) empty() bool {
	return len(s.Dirs) == 0 && len(s.Globs) == 0 && len(s.Files) == 0
}

// sensitiveSources lists the human-readable names of every source marked
// Sensitive, for the plaintext-repository refusal message.
func (s ExtrasSpec) sensitiveSources() []string {
	var names []string
	for _, d := range s.Dirs {
		if d.Sensitive {
			names = append(names, d.Name+string(filepath.Separator))
		}
	}
	for _, g := range s.Globs {
		if g.Sensitive {
			names = append(names, g.Pattern)
		}
	}
	for _, f := range s.Files {
		if f.Sensitive {
			names = append(names, f.RecordAs)
		}
	}
	return names
}

// ExtrasOptions parameterizes one extras capture (docs/usage/backup.md,
// Extras).
type ExtrasOptions struct {
	// DataDir anchors the spec's Dirs walks and Globs matches. Empty means
	// no directory- or glob-based extras are captured.
	DataDir               string
	Spec                  ExtrasSpec
	AllowPlaintextSecrets bool
	Encrypted             bool
	// ContentDirName/DBFileName name the restore-layout paths an extras
	// record path may never claim — the database, its SQLite sidecars, and
	// the attachments tree — so capture refuses them with the same rules
	// restore enforces (validateExtrasEntryPath) instead of publishing a
	// snapshot restore and verify then reject. Create fills them from the
	// App; a caller leaving them empty skips only the reserved-name rule
	// (locality and Windows-safe component rules always apply).
	ContentDirName string
	DBFileName     string
}

// ExtrasEntry is one captured file in the extras tree.
type ExtrasEntry struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
	Blob string `json:"blob"`
}

// ExtrasTree is the small JSON tree object referenced by the manifest.
type ExtrasTree struct {
	Entries []ExtrasEntry `json:"entries"`
}

// enterExtrasDir opens one directory component beneath cur as its own root,
// refusing symlinks: the component must lstat as a real directory and the
// opened descriptor must be that same inode, so a symlink present before the
// lstat and one raced in before the open both fail instead of being followed.
func enterExtrasDir(cur *os.Root, comp string) (*os.Root, error) {
	li, err := cur.Lstat(comp)
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
		return nil, fmt.Errorf("path component %q is not a real directory", comp)
	}
	sub, err := cur.OpenRoot(comp)
	if err != nil {
		return nil, err
	}
	held, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	if !os.SameFile(li, held) {
		_ = sub.Close()
		return nil, fmt.Errorf("path component %q changed during capture", comp)
	}
	return sub, nil
}

// openExtrasLeafNoFollow opens one file directly inside dir, refusing a
// symlink at the name: the lstat rejects one present up front, and SameFile
// against the opened descriptor rejects one raced in between lstat and open.
// The caller owns the returned file.
func openExtrasLeafNoFollow(dir *os.Root, name string) (*os.File, os.FileInfo, error) {
	li, err := dir.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q is not a regular file", name)
	}
	f, err := dir.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	if !os.SameFile(li, info) {
		return nil, nil, errors.Join(fmt.Errorf("%q changed during capture", name), f.Close())
	}
	return f, info, nil
}

// openExtrasNoFollow opens rel beneath root with symlink resolution refused
// at every path component. Unlike attachment reads — whose bytes are
// hash-verified against the database's recorded content hash, so a raced-in
// symlink yields a loud mismatch — extras have no expected hash: a symlink
// swapped in after the capture walk's lstat check could silently pull other
// in-DataDir files (tokens, client secrets) into the snapshot under a
// deletions path even when their capture is disabled. os.Root only refuses
// symlinks that escape the root, so each directory component is entered
// through its own verified sub-root and the leaf is tied to its lstat by
// descriptor identity; a swap at any component fails closed instead of being
// followed.
func openExtrasNoFollow(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) {
		return nil, nil, fmt.Errorf("extras path %q is not local", rel)
	}
	cur := root
	defer func() {
		if cur != root {
			_ = cur.Close()
		}
	}()
	comps := strings.Split(filepath.ToSlash(rel), "/")
	for _, comp := range comps[:len(comps)-1] {
		sub, err := enterExtrasDir(cur, comp)
		if err != nil {
			return nil, nil, err
		}
		if cur != root {
			_ = cur.Close()
		}
		cur = sub
	}
	return openExtrasLeafNoFollow(cur, comps[len(comps)-1])
}

// CaptureExtras streams extras files and stores their tree and chunk recipes.
// Cancellation is checked throughout each file read.
// An empty Tree means the selection contained no files. The returned recipes
// must be included in the snapshot's Extras; they require reader version 6.
func CaptureExtras(ctx context.Context, opts ExtrasOptions, appender *PackAppender) (ManifestExtras, error) {
	if err := ctx.Err(); err != nil {
		return ManifestExtras{}, err
	}
	if opts.Spec.empty() {
		return ManifestExtras{}, nil
	}
	// Sensitive sources (application secrets: tokens, credentials, config
	// files carrying API keys) on an unencrypted repository need the explicit
	// plaintext override. Fail safe by naming what triggered the guard.
	if sensitive := opts.Spec.sensitiveSources(); len(sensitive) > 0 &&
		!opts.Encrypted && !opts.AllowPlaintextSecrets {
		return ManifestExtras{}, fmt.Errorf(
			"backup: capturing %s requires an encrypted repository "+
				"(set AllowPlaintextSecrets to override)",
			strings.Join(sensitive, ", "))
	}
	// The spec's directory walks and glob matches all read files under
	// DataDir; route every read through one root confined to it so a regular
	// file swapped for a symlink between the walk/glob check and the read (a
	// TOCTOU the plain os.ReadFile below used to lose) cannot pull a host file
	// from outside DataDir into the extras blob. os.Root refuses any path that
	// escapes the root, and openExtrasNoFollow additionally refuses symlink
	// resolution at every path component, so a raced-in symlink cannot
	// redirect the read even to a target still inside DataDir — extras bytes
	// carry no expected hash, so unlike attachment reads a redirect would be
	// silent.
	var dataRoot *os.Root
	if opts.DataDir != "" {
		dr, err := os.OpenRoot(opts.DataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ManifestExtras{}, fmt.Errorf("backup: opening data dir for extras: %w", err)
		}
		if err == nil {
			dataRoot = dr
			defer func() { _ = dataRoot.Close() }()
		}
	}
	var entries []ExtrasEntry
	var recipes []string
	seen := map[string]string{}
	// addFile reads recordPath's bytes through readRoot with no symlink
	// resolution at any component (openExtrasNoFollow) and records the tree
	// entry under recordPath. readRel is recordPath's location relative to
	// readRoot. A record path selected twice (overlapping globs, a Files spec
	// duplicating a walk), or two paths that fold to one file on a
	// case-insensitive filesystem, are refused with the same folded key
	// restore's checkExtrasCollisions uses: restore rejects such trees, so
	// capturing one would produce an unrestorable snapshot.
	addFile := func(readRoot *os.Root, readRel, recordPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Every record path is held to restore's rules at capture time:
		// restore and verify reject escaping, reserved-overlapping, and
		// Windows-aliasing paths, so recording one would publish a snapshot
		// that can never be restored.
		if _, err := validateExtrasEntryPath(recordPath, opts.ContentDirName, opts.DBFileName); err != nil {
			return err
		}
		key := foldedPathKey(recordPath)
		slashPath := filepath.ToSlash(recordPath)
		if other, dup := seen[key]; dup {
			if other == slashPath {
				return fmt.Errorf("backup: extras record path %q is selected more than once", slashPath)
			}
			return fmt.Errorf(
				"backup: extras record paths %q and %q would collide on a case-insensitive filesystem; restore refuses such snapshots",
				other, slashPath)
		}
		seen[key] = slashPath
		file, info, err := openExtrasNoFollow(readRoot, readRel)
		if err != nil {
			return fmt.Errorf("backup: reading extras file %s: %w", recordPath, err)
		}
		id, size, recipe, captureErr := captureObject(ctx, file, info.Size(), nil, appender)
		if err := errors.Join(captureErr, file.Close()); err != nil {
			return fmt.Errorf("backup: capturing extras file %s: %w", recordPath, err)
		}
		if recipe != "" {
			recipes = append(recipes, recipe)
		}
		entries = append(entries, ExtrasEntry{
			Path: filepath.ToSlash(recordPath),
			Mode: uint32(info.Mode().Perm()),
			Size: size,
			Blob: id.String(),
		})
		return nil
	}
	addDir := func(name string) error {
		if opts.DataDir == "" {
			return nil
		}
		root := filepath.Join(opts.DataDir, name)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("backup: walking %s: %w", name, err)
			}
			if d.IsDir() {
				return nil
			}
			// Reject non-regular files: symlinks, sockets, devices, etc.
			// Symlinks are an attack surface — they could point outside DataDir.
			if !d.Type().IsRegular() {
				if d.Type()&fs.ModeSymlink != 0 {
					rel, err := filepath.Rel(opts.DataDir, path)
					if err != nil {
						rel = path
					}
					return fmt.Errorf("extras: %s is not a regular file", filepath.ToSlash(rel))
				}
				// Skip other non-regular types silently.
				return nil
			}
			rel, err := filepath.Rel(opts.DataDir, path)
			if err != nil {
				return err
			}
			return addFile(dataRoot, rel, rel)
		})
	}
	for _, d := range opts.Spec.Dirs {
		if err := addDir(d.Name); err != nil {
			return ManifestExtras{}, err
		}
	}
	for _, f := range opts.Spec.Files {
		if !filepath.IsLocal(filepath.FromSlash(f.RecordAs)) {
			return ManifestExtras{}, fmt.Errorf(
				"backup: extras file record path %q is not a local relative path", f.RecordAs)
		}
		// The file may live outside DataDir, so confine the read to its own
		// parent directory: a symlink swapped in at the path is then refused
		// rather than followed to an arbitrary host file.
		fileRoot, err := os.OpenRoot(filepath.Dir(f.Path))
		if err != nil {
			return ManifestExtras{}, fmt.Errorf("backup: opening extras file dir for %s: %w", f.RecordAs, err)
		}
		err = addFile(fileRoot, filepath.Base(f.Path), f.RecordAs)
		_ = fileRoot.Close()
		if err != nil {
			return ManifestExtras{}, err
		}
	}
	// Glob matching only makes sense with a confined root to read through:
	// with no DataDir there is nothing to scan, and addFile would nil-deref
	// on the absent dataRoot.
	if len(opts.Spec.Globs) > 0 && dataRoot != nil {
		// Match by basename through the confined root instead of
		// filepath.Glob(filepath.Join(DataDir, ...)): a DataDir path containing
		// glob metacharacters ([, *, ?) would otherwise make Glob silently skip
		// real matches or pull in siblings. Patterns are pure basenames with
		// no directory part, so they never interact with the data dir path.
		dirEntries, err := fs.ReadDir(dataRoot.FS(), ".")
		if err != nil {
			return ManifestExtras{}, fmt.Errorf("backup: reading data dir for extras globs: %w", err)
		}
		for _, g := range opts.Spec.Globs {
			if strings.ContainsAny(g.Pattern, `/\`) {
				return ManifestExtras{}, fmt.Errorf(
					"backup: extras glob pattern %q must be a pure basename", g.Pattern)
			}
			for _, e := range dirEntries {
				name := e.Name()
				match, err := filepath.Match(g.Pattern, name)
				if err != nil {
					return ManifestExtras{}, fmt.Errorf("backup: extras glob %q: %w", g.Pattern, err)
				}
				if !match {
					continue
				}
				// ReadDir reports the entry's own type (Lstat semantics, not
				// followed), so a symlink or non-regular file matching the
				// pattern yields a friendly early error; the no-follow read
				// through dataRoot is the authoritative guard against a
				// symlink raced in after this check.
				if !e.Type().IsRegular() {
					return ManifestExtras{}, fmt.Errorf("extras: %s is not a regular file", name)
				}
				if err := addFile(dataRoot, name, name); err != nil {
					return ManifestExtras{}, err
				}
			}
		}
	}
	if len(entries) == 0 {
		return ManifestExtras{}, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.MarshalIndent(&ExtrasTree{Entries: entries}, "", "  ")
	if err != nil {
		return ManifestExtras{}, fmt.Errorf("backup: marshaling extras tree: %w", err)
	}
	id, _, err := appender.Add(data)
	if err != nil {
		return ManifestExtras{}, err
	}
	return ManifestExtras{Tree: id.String(), Recipes: recipes}, nil
}
