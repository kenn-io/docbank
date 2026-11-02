// Package backup is a snapshot engine for applications whose state consists
// of metadata plus a tree of content-addressed files. Metadata may be captured
// either as incremental SQLite page maps or as an application-defined portable
// stream that the application rebuilds into its current runtime database on
// restore.
//
// Create pins the database in a frozen read transaction, captures changed
// SQLite pages and any new content files into content-addressed packs
// (kit/pack), and writes a manifest chained to the previous snapshot. Restore
// and Verify reconstruct and validate snapshots from the repository. Plain
// content capture, verification, and loose restore use bounded-memory streams;
// format-v1 encrypted entries retain their whole-entry authentication contract
// and are not exposed as verified-prefix streams.
//
// Existing Create, Verify, and Restore callers receive the plain-content
// streaming implementation without changing application adapters. Snapshots
// containing large logical objects use version-5 chunk recipes. Repo.OpenBlob
// is available to sequential consumers; bytes from it
// become authoritative only after terminal EOF or a successful Verify, and an
// early Close reports incomplete verification. Capture preparation trades
// object-sized heap for private repository scratch, so callers must provision
// scratch capacity for the configured capture concurrency.
//
// The engine is application-neutral: everything specific to a given
// application (its database filename, content directory, referenced-file
// enumeration, portable metadata format, and the opaque stats payload recorded
// per snapshot) is supplied through application interfaces. The engine records
// stats bytes at create and byte-compares them at restore without interpreting
// them.
//
// On-disk formats are versioned by the FormatVersion and MinReaderVersion
// constants and the per-snapshot manifest fields; readers refuse snapshots
// that require a newer reader.
package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// FormatVersion is the backup repository format version this code writes.
	FormatVersion = 1
	// MinReaderVersion is the oldest reader able to read repos this code
	// writes.
	MinReaderVersion = 1
	// SupportedReaderVersion is the newest format this code can read. It is
	// deliberately distinct from FormatVersion ("what we write"): a future
	// release may read formats newer than the one it writes, or vice versa.
	// Repo.Open and LoadManifest refuse anything whose min_reader_version
	// exceeds this. Version 7 is a repository gate an application can raise
	// with Repo.RequireReaderVersion; it changes no layout.
	SupportedReaderVersion = 7

	// dbPathManifestVersion marks snapshots whose attachment population
	// records storage paths beyond the canonical loose "<aa>/<hash>"
	// derivation. Version-1 readers restored every attachment to the
	// canonical path only, so restoring such a snapshot with one would
	// "succeed" while the database points at files that do not exist; the
	// manifest version bump turns that into an explicit refusal. Snapshots
	// whose paths are all canonical keep version 1 and stay readable by
	// older code.
	dbPathManifestVersion = 2
	// portableMetadataManifestVersion marks snapshots whose application state
	// is a logical metadata blob rather than SQLite page-map chains.
	portableMetadataManifestVersion = 3
	// auxiliaryManifestVersion marks snapshots carrying application-defined
	// auxiliary artifacts alongside logical metadata and content.
	auxiliaryManifestVersion = 4
	// chunkedObjectManifestVersion marks snapshots with logical-object recipes.
	chunkedObjectManifestVersion = 5
	// chunkedExtrasManifestVersion extends recipes to operational extras.
	chunkedExtrasManifestVersion = 6

	repoConfigName   = "config.toml"
	snapshotsDirName = "snapshots"
	packsDirName     = "packs"
	indexesDirName   = "indexes"
	locksDirName     = "locks"
	stagingDirName   = "staging"
	keysDirName      = "keys"
)

// RepoConfig is the plaintext repository descriptor (FORMAT.md). It stays
// unencrypted even in encrypted repos because it bootstraps everything else.
type RepoConfig struct {
	RepoID           string `toml:"repo_id"`
	FormatVersion    int    `toml:"format_version"`
	MinReaderVersion int    `toml:"min_reader_version"`
	Encryption       string `toml:"encryption"`
	CreatedAt        string `toml:"created_at"`
	PageSize         int    `toml:"page_size"`
}

// Repo is an opened backup repository rooted at a directory.
type Repo struct {
	root string
	cfg  RepoConfig
}

// Init creates a new empty repository at root. It refuses to reuse a
// directory that already contains a repository config.
func Init(root string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(root, repoConfigName)); err == nil {
		return nil, fmt.Errorf("backup: repository already initialized at %s",
			root)
	}
	for _, dir := range []string{
		root,
		filepath.Join(root, snapshotsDirName),
		filepath.Join(root, packsDirName),
		filepath.Join(root, indexesDirName),
		filepath.Join(root, locksDirName),
		filepath.Join(root, stagingDirName),
		filepath.Join(root, keysDirName),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("backup: creating %s: %w", dir, err)
		}
	}
	cfg := RepoConfig{
		RepoID:           newRepoID(),
		FormatVersion:    FormatVersion,
		MinReaderVersion: MinReaderVersion,
		Encryption:       "none",
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	r := &Repo{root: root, cfg: cfg}
	if err := r.writeConfig(r.cfg); err != nil {
		return nil, err
	}
	return r, nil
}

// Open loads an existing repository and enforces version compatibility.
func Open(root string) (*Repo, error) {
	data, err := os.ReadFile(filepath.Join(root, repoConfigName))
	if err != nil {
		return nil, fmt.Errorf("backup: opening repository at %s: %w", root,
			err)
	}
	var cfg RepoConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("backup: parsing repository config: %w", err)
	}
	if cfg.MinReaderVersion > SupportedReaderVersion {
		return nil, fmt.Errorf(
			"backup: repository requires reader version %d but this "+
				"reader supports %d; upgrade the reader",
			cfg.MinReaderVersion, SupportedReaderVersion)
	}
	if cfg.Encryption != "none" {
		return nil, fmt.Errorf(
			"backup: encrypted repositories are not supported yet "+
				"(encryption=%q)", cfg.Encryption)
	}
	// The repo ID is used verbatim as a filename component (the local
	// hash-map cache), so a tampered config.toml carrying path separators or
	// traversal sequences must be refused here, before any caller joins it
	// into a path.
	if !validRepoID(cfg.RepoID) {
		return nil, fmt.Errorf(
			"backup: repository config repo_id %q is not the canonical UUID "+
				"form; refusing a tampered or hand-edited config.toml",
			cfg.RepoID)
	}
	return &Repo{root: root, cfg: cfg}, nil
}

// Root returns the repository root directory.
func (r *Repo) Root() string { return r.root }

// Config returns the repository descriptor.
func (r *Repo) Config() RepoConfig { return r.cfg }

// Path joins parts under the repository root.
func (r *Repo) Path(parts ...string) string {
	return filepath.Join(append([]string{r.root}, parts...)...)
}

// packPath returns the sharded on-disk path for pack id, using ext (the
// application's App.PackFileExtension) as the file extension. It is the
// single place in the engine that assembles a pack's location; ext is
// validated at each flow's entry (validatePackExtension) before any path is
// built from it.
func (r *Repo) packPath(id, ext string) string {
	return r.Path(packsDirName, id[:2], id+ext)
}

// validatePackExtension rejects an App.PackFileExtension the pack layout
// cannot host safely: anything but a leading dot followed by dot-free,
// separator-free characters. packPath concatenates the extension straight
// into a path component, so a separator or ".." would place packs outside
// the repository's packs tree; an extra dot would make pack.OpenReader
// derive the wrong pack ID (it strips only the final filepath.Ext), which
// for encrypted packs breaks the footer's AAD binding to the ID. Create,
// Verify, and Restore each check this once up front, so a misconfigured
// application fails loudly before any pack is written or read.
func validatePackExtension(ext string) error {
	if len(ext) < 2 || ext[0] != '.' {
		return fmt.Errorf(
			"backup: pack file extension %q must be a leading dot followed by a suffix (e.g. %q)", ext, ".kpack")
	}
	if strings.ContainsAny(ext[1:], `./\:`) {
		return fmt.Errorf(
			"backup: pack file extension %q must not contain dots, path separators, or colons after the leading dot", ext)
	}
	return nil
}

// SetPageSize records the DB page size after the first backup. The caller
// must hold the exclusive repo lock. It re-reads config.toml first, so it
// fails on a repository this build cannot open.
func (r *Repo) SetPageSize(pageSize int) error {
	return r.updateConfig(func(cfg *RepoConfig) bool {
		if cfg.PageSize == pageSize {
			return false
		}
		cfg.PageSize = pageSize
		return true
	})
}

// RequireReaderVersion raises the repository's min_reader_version to v so
// that builds whose SupportedReaderVersion is below v refuse the repository
// at Open. It never lowers the value and returns nil when the recorded one is
// already at least v. It errors when v exceeds SupportedReaderVersion, since
// this build could not open the repository afterwards. The gate applies at
// Open only: a build that already holds a handle keeps using it, and an
// older build's SetPageSize may write its cached, lower value back. The
// caller must hold
// the exclusive repo lock. It re-reads config.toml first, so it fails on a
// repository this build cannot open.
func (r *Repo) RequireReaderVersion(v int) error {
	if v > SupportedReaderVersion {
		return fmt.Errorf(
			"backup: cannot require reader version %d: this reader "+
				"supports %d", v, SupportedReaderVersion)
	}
	return r.updateConfig(func(cfg *RepoConfig) bool {
		if cfg.MinReaderVersion >= v {
			return false
		}
		cfg.MinReaderVersion = v
		return true
	})
}

// updateConfig reloads the config from disk, so a field another handle
// changed since this one was opened is not written back from a stale copy,
// then applies change to a copy and publishes it when change reports true.
// The handle takes the changed config only after the write succeeds, so a
// failed write leaves it at what was last read from disk. It refuses a config
// whose repo_id differs from the handle's: the directory now holds another
// repository, and adopting its ID would key local caches to the wrong one.
func (r *Repo) updateConfig(change func(*RepoConfig) bool) error {
	fresh, err := Open(r.root)
	if err != nil {
		return err
	}
	if fresh.cfg.RepoID != r.cfg.RepoID {
		return fmt.Errorf(
			"backup: repository at %s is now %s, not %s; reopen it",
			r.root, fresh.cfg.RepoID, r.cfg.RepoID)
	}
	// Assign only on change so an unchanged config, the common case on
	// every Create, does not write the handle.
	if fresh.cfg != r.cfg {
		r.cfg = fresh.cfg
	}
	next := r.cfg
	if !change(&next) {
		return nil
	}
	if err := r.writeConfig(next); err != nil {
		return err
	}
	r.cfg = next
	return nil
}

func (r *Repo) writeConfig(cfg RepoConfig) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("backup: encoding repository config: %w", err)
	}
	return writeFileAtomic(r, repoConfigName, buf.Bytes())
}

// CleanStaging removes in-flight write debris. Callers must hold the
// exclusive repo lock (concurrent writers stage under the same directory).
func (r *Repo) CleanStaging() error {
	staging := r.Path(stagingDirName)
	info, err := os.Lstat(staging)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(staging, 0o700)
	case err != nil:
		return fmt.Errorf("backup: checking staging dir: %w", err)
	case !info.IsDir():
		// Lstat never follows symlinks, so a "staging" symlink planted by
		// another principal reports ModeSymlink here instead of the
		// target's mode. Refuse rather than RemoveAll entries wherever it
		// points.
		return fmt.Errorf("backup: staging path %s is not a directory; refusing to clean it", staging)
	}
	// The enumeration and removals must not resolve the staging path again: a
	// symlink swapped in after the Lstat above would send them into its
	// target. Hold a descriptor-confined root, prove it is the same directory
	// the Lstat saw (OpenRoot follows a final-component symlink; SameFile
	// against the lstat'd inode closes that race), and remove entries through
	// the root only.
	root, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("backup: opening staging dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	held, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("backup: checking staging dir: %w", err)
	}
	if !os.SameFile(info, held) {
		return fmt.Errorf(
			"backup: staging path %s was replaced while opening it; refusing to clean it", staging)
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("backup: reading staging dir: %w", err)
	}
	for _, e := range entries {
		if err := root.RemoveAll(e.Name()); err != nil {
			return fmt.Errorf("backup: cleaning staging entry %s: %w",
				e.Name(), err)
		}
	}
	return nil
}

// repoIDPattern is the exact shape newRepoID generates: a lowercase-hex
// UUID. Anything else in a config.toml is tampering or hand-editing.
var repoIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// validRepoID reports whether id is a canonical generated repository ID and
// therefore safe to embed in filenames.
func validRepoID(id string) bool { return repoIDPattern.MatchString(id) }

func newRepoID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("backup: reading random bytes: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10],
		b[10:16])
}
