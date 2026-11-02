package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
)

// Manifest types (FORMAT.md)

type Manifest struct {
	FormatVersion    int `json:"format_version"`
	MinReaderVersion int `json:"min_reader_version"`
	// AppVersion records the application version that wrote the snapshot. The
	// wire key is frozen at format v1: renaming it would break every existing
	// repo's snapshot-ID recompute.
	AppVersion  string              `json:"msgvault_version"`
	SnapshotID  string              `json:"snapshot_id"`
	ParentID    string              `json:"parent_id"`
	CreatedAt   string              `json:"created_at"`
	Options     ManifestOptions     `json:"options"`
	DB          ManifestDB          `json:"db"`
	Metadata    *ManifestMetadata   `json:"metadata,omitempty"`
	Auxiliary   []ManifestAuxiliary `json:"auxiliary,omitempty"`
	Attachments ManifestAttachments `json:"attachments"`
	Extras      ManifestExtras      `json:"extras"`
	Excluded    []string            `json:"excluded"`
	// Stats is the application-defined stats payload. The engine treats it as
	// opaque bytes: recorded at create, byte-compared at restore.
	Stats           json.RawMessage `json:"stats"`
	NewPacks        []string        `json:"new_packs"`
	NewIndex        string          `json:"new_index"`
	DurationSeconds float64         `json:"duration_seconds"`
	BytesAdded      int64           `json:"bytes_added"`
}

// ManifestMetadata identifies one application-owned portable metadata blob.
// nil denotes the historical SQLite page-map representation in DB.
type ManifestMetadata struct {
	Format string `json:"format"`
	Blob   string `json:"blob"`
	Bytes  int64  `json:"bytes"`
	Recipe string `json:"recipe,omitempty"`
}

func validatePortableManifest(m *Manifest) error {
	if m.Metadata == nil {
		return nil
	}
	if m.FormatVersion < portableMetadataManifestVersion ||
		m.MinReaderVersion < portableMetadataManifestVersion {
		return fmt.Errorf(
			"backup: portable metadata requires manifest and reader version %d",
			portableMetadataManifestVersion)
	}
	if m.DB != (ManifestDB{}) {
		return errors.New("backup: portable metadata manifest also carries SQLite page-map authority")
	}
	if err := validateMetadataFormat(m.Metadata.Format); err != nil {
		return err
	}
	if m.Metadata.Bytes < 0 {
		return fmt.Errorf("backup: portable metadata has negative size %d", m.Metadata.Bytes)
	}
	if _, err := pack.ParseBlobID(m.Metadata.Blob); err != nil {
		return fmt.Errorf("backup: portable metadata blob id %q: %w", m.Metadata.Blob, err)
	}
	return nil
}

type ManifestOptions struct {
	IncludeConfig bool   `json:"include_config"`
	IncludeTokens bool   `json:"include_tokens"`
	ZstdLevel     int    `json:"zstd_level"`
	Tag           string `json:"tag"`
}

type ManifestDB struct {
	Engine        string `json:"engine"`
	PageSize      uint32 `json:"page_size"`
	PageCount     uint64 `json:"page_count"`
	PageMap       string `json:"page_map"`
	PageHashMap   string `json:"page_hash_map"`
	MapChainDepth int    `json:"map_chain_depth"`
}

type ManifestAttachments struct {
	Layout    []string `json:"layout"`
	Rows      int64    `json:"rows"`
	Blobs     int64    `json:"blobs"`
	BlobBytes int64    `json:"blob_bytes"`
	Recipes   []string `json:"recipes"`
	Lists     []string `json:"lists"`
}

type ManifestExtras struct {
	Tree    string   `json:"tree"`
	Recipes []string `json:"recipes,omitempty"`
}

const manifestExt = ".mvmanifest"

// ComputeSnapshotID derives the time-ordered, content-derived snapshot ID
// (FORMAT.md): UTC timestamp plus the first 32 hex
// chars (128 bits) of the SHA-256 of the manifest JSON with snapshot_id
// blanked. The digest must be long enough that crafting a different
// manifest with the same ID is infeasible: LoadManifest's recompute check
// is what stops a forged manifest from being served under a known snapshot
// ID, and a short, brute-forceable suffix would defeat it.
func ComputeSnapshotID(createdAt time.Time, m *Manifest) (string, error) {
	cp := *m
	cp.SnapshotID = ""
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("backup: marshaling manifest for snapshot id: %w", err)
	}
	sum := sha256.Sum256(data)
	return createdAt.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(sum[:16]), nil
}

// WriteManifest fills the snapshot ID and publishes the manifest. It must be
// the final write of a backup: a manifest's existence asserts closure.
func (r *Repo) WriteManifest(m *Manifest) (string, error) {
	if err := validateObjectManifest(m); err != nil {
		return "", err
	}
	if err := validateAuxiliaryManifest(m); err != nil {
		return "", err
	}
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return "", fmt.Errorf("backup: manifest created_at %q: %w", m.CreatedAt, err)
	}
	id, err := ComputeSnapshotID(createdAt, m)
	if err != nil {
		return "", err
	}
	m.SnapshotID = id
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("backup: marshaling manifest: %w", err)
	}
	if err := writeFileAtomic(r, filepath.Join(snapshotsDirName, id+manifestExt), data); err != nil {
		return "", err
	}
	return id, nil
}

// snapshotIDPattern is the exact shape ComputeSnapshotID generates: a UTC
// second timestamp plus 32 lowercase-hex digest chars.
var snapshotIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{32}$`)

// LoadManifest reads one manifest by snapshot ID.
func (r *Repo) LoadManifest(id string) (*Manifest, error) {
	return loadManifest(os.DirFS(r.Path(snapshotsDirName)), id)
}

func loadManifest(snapshots fs.FS, id string) (*Manifest, error) {
	// The ID becomes a filename under snapshots/, and it arrives from
	// untrusted places — a caller's RestoreOptions.SnapshotID, or the
	// parent_id of a manifest in a synced-in repository — so anything but
	// the generated shape is refused before it touches a path. Without this,
	// an ID carrying "../" would read *.mvmanifest paths outside the
	// repository before the content-derived ID check could fail it.
	if !snapshotIDPattern.MatchString(id) {
		return nil, fmt.Errorf("backup: snapshot id %q is not a valid snapshot ID", id)
	}
	data, err := fs.ReadFile(snapshots, id+manifestExt)
	if err != nil {
		return nil, fmt.Errorf("backup: loading snapshot %s: %w", id, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("backup: parsing snapshot %s: %w", id, err)
	}
	// Unknown JSON fields are silently ignored above, so a manifest written
	// by a newer format could otherwise be misread as current (e.g. an
	// encrypted snapshot treated as plaintext). The per-snapshot
	// min_reader_version gate turns that into an explicit refusal. It must
	// run before the strict re-decode and the ID check below: a newer
	// manifest must produce the actionable "upgrade the reader" error, not
	// an unknown-field or corruption one.
	if m.MinReaderVersion > SupportedReaderVersion {
		return nil, fmt.Errorf(
			"backup: snapshot %s requires reader version %d but this "+
				"reader supports %d; upgrade the reader",
			id, m.MinReaderVersion, SupportedReaderVersion)
	}
	// A manifest claiming to be readable by this version must contain only
	// fields this version knows: the ID recompute below hashes the parsed
	// struct, so an unknown field would ride along in the file without
	// affecting the ID — letting arbitrary data be smuggled into a manifest
	// that still authenticates, where a newer reader (which does know the
	// field) would trust it. Unknown fields inside the opaque Stats payload
	// stay allowed: RawMessage captures them verbatim and the ID covers them.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var strict Manifest
	if err := dec.Decode(&strict); err != nil {
		return nil, fmt.Errorf(
			"backup: snapshot %s carries fields this reader does not know "+
				"but claims min_reader_version %d; the manifest is forged or "+
				"its writer failed to bump the version: %w",
			id, m.MinReaderVersion, err)
	}
	m = strict
	// The snapshot ID is content-derived, so recomputing it authenticates
	// every manifest field against the filename. Without this, corrupted or
	// hand-edited manifest metadata would be accepted by list, latest, and
	// verify.
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot %s created_at %q: %w", id, m.CreatedAt, err)
	}
	computed, err := ComputeSnapshotID(createdAt, &m)
	if err != nil {
		return nil, err
	}
	if computed != id || m.SnapshotID != id {
		return nil, fmt.Errorf(
			"backup: snapshot %s failed its content-derived ID check "+
				"(computed %s, embedded %q); the manifest file is corrupted, renamed, or forged",
			id, computed, m.SnapshotID)
	}
	if err := validateObjectManifest(&m); err != nil {
		return nil, fmt.Errorf("backup: snapshot %s: %w", id, err)
	}
	if err := validatePortableManifest(&m); err != nil {
		return nil, fmt.Errorf("backup: snapshot %s: %w", id, err)
	}
	if err := validateAuxiliaryManifest(&m); err != nil {
		return nil, fmt.Errorf("backup: snapshot %s: %w", id, err)
	}
	return &m, nil
}

func validateAuxiliaryManifest(m *Manifest) error {
	if len(m.Auxiliary) > 0 && (m.FormatVersion < auxiliaryManifestVersion ||
		m.MinReaderVersion < auxiliaryManifestVersion) {
		return fmt.Errorf(
			"backup: auxiliary artifacts require manifest and reader version %d",
			auxiliaryManifestVersion,
		)
	}
	return validateManifestAuxiliary(m.Auxiliary)
}

// ListSnapshots returns every manifest sorted ascending by snapshot ID
// (IDs are time-prefixed, so this is chronological). Create enforces
// strictly increasing CreatedAt timestamps per repo (see nextCreatedAt in
// create.go), so even snapshots created within the same wall-clock second
// still sort chronologically by ID. Lock-free by design.
func (r *Repo) ListSnapshots() ([]*Manifest, error) {
	return listSnapshots(os.DirFS(r.Path(snapshotsDirName)))
}

func listSnapshots(snapshots fs.FS) ([]*Manifest, error) {
	entries, err := fs.ReadDir(snapshots, ".")
	if err != nil {
		return nil, fmt.Errorf("backup: reading snapshots dir: %w", err)
	}
	var out []*Manifest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), manifestExt) {
			continue
		}
		m, err := loadManifest(snapshots, strings.TrimSuffix(e.Name(), manifestExt))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SnapshotID < out[j].SnapshotID })
	return out, nil
}

// LatestSnapshot returns the newest manifest, or nil for an empty repo.
func (r *Repo) LatestSnapshot() (*Manifest, error) {
	list, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[len(list)-1], nil
}

// HashMapChain collects the newest-to-oldest page-hash-map blob chain from
// head down to (and including) its keyframe manifest.
func (r *Repo) HashMapChain(head *Manifest) ([]pack.BlobID, error) {
	return r.mapChain(head, func(m *Manifest) string { return m.DB.PageHashMap })
}

// PageMapChain collects the newest-to-oldest page-map blob chain.
func (r *Repo) PageMapChain(head *Manifest) ([]pack.BlobID, error) {
	return r.mapChain(head, func(m *Manifest) string { return m.DB.PageMap })
}

func (r *Repo) mapChain(head *Manifest, field func(*Manifest) string) ([]pack.BlobID, error) {
	manifests, err := r.manifestChain(head)
	if err != nil {
		return nil, err
	}
	var chain []pack.BlobID
	for _, m := range manifests {
		id, err := pack.ParseBlobID(field(m))
		if err != nil {
			return nil, fmt.Errorf("backup: snapshot %s map blob: %w", m.SnapshotID, err)
		}
		chain = append(chain, id)
	}
	return chain, nil
}

// manifestChain is the recovery dependency chain, newest to keyframe.
// Portable metadata is self-contained; its ParentID is informational.
func (r *Repo) manifestChain(head *Manifest) ([]*Manifest, error) {
	return walkManifestChain(head, r.LoadManifest)
}

func walkManifestChain(head *Manifest, load func(string) (*Manifest, error)) ([]*Manifest, error) {
	var chain []*Manifest
	m := head
	visited := make(map[string]struct{})
	iterations := 0

	for {
		chain = append(chain, m)
		if m.Metadata != nil || m.DB.MapChainDepth == 0 {
			return chain, nil
		}

		// Detect cycles. LoadManifest's content-derived ID check makes an
		// on-disk parent cycle infeasible to construct (each ID would have to
		// be a SHA-256 fixed point over the other's), so this is pure
		// defense-in-depth behind the iteration cap below.
		if _, seen := visited[m.SnapshotID]; seen {
			return nil, fmt.Errorf("backup: snapshot chain cycle at %s", m.SnapshotID)
		}
		visited[m.SnapshotID] = struct{}{}

		// Enforce depth limit
		iterations++
		if iterations > keyframeChainMax {
			return nil, fmt.Errorf("backup: snapshot chain exceeds limit of %d", keyframeChainMax)
		}

		if m.ParentID == "" {
			return nil, fmt.Errorf("backup: snapshot %s has chain depth %d but no parent", m.SnapshotID, m.DB.MapChainDepth)
		}
		parent, err := load(m.ParentID)
		if err != nil {
			return nil, fmt.Errorf("backup: walking map chain: %w", err)
		}
		m = parent
	}
}

func validateObjectManifest(m *Manifest) error {
	if len(m.Extras.Recipes) > 0 &&
		(m.FormatVersion < chunkedExtrasManifestVersion || m.MinReaderVersion < chunkedExtrasManifestVersion) {
		return errors.New("backup: extras recipes require manifest and reader version 6")
	}
	ids := append([]string(nil), m.Attachments.Recipes...)
	ids = append(ids, m.Extras.Recipes...)
	if m.Metadata != nil && m.Metadata.Recipe != "" {
		ids = append(ids, m.Metadata.Recipe)
	}
	if len(ids) == 0 {
		return nil
	}
	if m.FormatVersion < chunkedObjectManifestVersion || m.MinReaderVersion < chunkedObjectManifestVersion {
		return errors.New("backup: object recipes require manifest and reader version 5")
	}
	for _, id := range ids {
		if _, err := pack.ParseBlobID(id); err != nil {
			return fmt.Errorf("backup: invalid object recipe identity: %w", err)
		}
	}
	return nil
}
