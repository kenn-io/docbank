// Package qmdexport builds disposable QMD-compatible Markdown collections
// from catalog-authorized Docbank rendition artifacts.
package qmdexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

const (
	ManifestFormatV1        = "docbank-qmd-export/v1"
	defaultMaxDocuments     = 100_000
	defaultMaxDocumentBytes = int64(64 << 20)
	defaultMaxTotalBytes    = int64(4 << 30)
	publishLockRetry        = 25 * time.Millisecond
)

// Source is one catalog-authorized active sanitized-Markdown artifact.
type Source = store.QMDExportSource

// Entry maps one QMD URI back to exact Docbank authority.
type Entry struct {
	URI                          string `json:"uri"`
	RelativePath                 string `json:"relative_path"`
	VaultUID                     string `json:"vault_uid"`
	NodeID                       int64  `json:"node_id"`
	ContentVersionID             string `json:"content_version_id"`
	ProcessingProfileFingerprint string `json:"processing_profile_fingerprint"`
	AttachmentID                 string `json:"attachment_id"`
	BuildID                      string `json:"build_id"`
	ArtifactID                   string `json:"artifact_id"`
	BlobSHA256                   string `json:"blob_sha256"`
	BlobSize                     int64  `json:"blob_size"`
	ArtifactChecksum             string `json:"artifact_checksum"`
	MarkdownChecksum             string `json:"markdown_checksum"`
	ExportedMarkdownSHA256       string `json:"exported_markdown_sha256"`
	Frontmatter                  string `json:"frontmatter,omitempty"`
}

// Manifest is the deterministic identity map for one complete collection.
type Manifest struct {
	Format     string  `json:"format"`
	Collection string  `json:"collection"`
	Entries    []Entry `json:"entries"`
	Checksum   string  `json:"checksum"`
}

// Options bounds source discovery before any retained blob is opened.
type Options struct {
	MaxDocuments     int
	MaxDocumentBytes int64
	MaxTotalBytes    int64
	MaxManifestBytes int64
}

type publishHooks struct {
	afterCurrent  func()
	waitingOnLock func()
}

// Generation is a complete validated in-memory export ready for publication.
type Generation struct {
	ID        string
	Manifest  Manifest
	documents map[string][]byte
}

// Receipt identifies the immutable generation selected by CURRENT.
type Receipt struct {
	GenerationID   string
	CollectionPath string
	Manifest       Manifest
}

// BlobReader opens catalog-authorized loose or packed rendition bytes.
type BlobReader interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

// SourceCatalog lists one bounded snapshot of active export authority.
type SourceCatalog interface {
	QMDExportSources(ctx context.Context, limit int) ([]Source, error)
}

// PublishActive snapshots the catalog and publishes its complete current set.
func PublishActive(ctx context.Context, root, collection string, catalog SourceCatalog, reader BlobReader, options Options) (Receipt, error) {
	if catalog == nil {
		return Receipt{}, errors.New("qmd export requires a source catalog")
	}
	bounds, err := normalizeOptions(options)
	if err != nil {
		return Receipt{}, err
	}
	sources, err := catalog.QMDExportSources(ctx, bounds.MaxDocuments)
	if err != nil {
		return Receipt{}, fmt.Errorf("snapshot qmd export sources: %w", err)
	}
	return Publish(ctx, root, collection, sources, reader, bounds)
}

// Build reads and verifies every exact source before returning a generation.
func Build(ctx context.Context, collection string, sources []Source, reader BlobReader, options Options) (Generation, error) {
	if ctx == nil || reader == nil {
		return Generation{}, errors.New("qmd export requires context and blob reader")
	}
	if !validCollection(collection) {
		return Generation{}, errors.New("qmd export collection name is invalid")
	}
	bounds, err := normalizeOptions(options)
	if err != nil {
		return Generation{}, err
	}
	if len(sources) > bounds.MaxDocuments {
		return Generation{}, errors.New("qmd export document membership exceeds bound")
	}
	canonical := slices.Clone(sources)
	slices.SortFunc(canonical, compareSource)
	var total int64
	for index, source := range canonical {
		if err := validateSource(source); err != nil {
			return Generation{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
		if source.BlobSize > bounds.MaxDocumentBytes || source.BlobSize > bounds.MaxTotalBytes-total {
			return Generation{}, errors.New("qmd export source bytes exceed bound")
		}
		total += source.BlobSize
		if index > 0 && compareSource(canonical[index-1], source) == 0 {
			return Generation{}, errors.New("qmd export contains duplicate source authority")
		}
	}

	manifest := Manifest{Format: ManifestFormatV1, Collection: collection}
	documents := make(map[string][]byte, len(canonical))
	for index, source := range canonical {
		if err := ctx.Err(); err != nil {
			return Generation{}, err
		}
		markdown, err := readSource(ctx, reader, source)
		if err != nil {
			return Generation{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
		frontmatter, body := splitFrontmatter(markdown)
		identity := sourcePathIdentity(source)
		relative := "documents/" + identity[:2] + "/" + identity + ".md"
		if _, exists := documents[relative]; exists {
			return Generation{}, errors.New("qmd export synthetic path collision")
		}
		documents[relative] = body
		exportedDigest := sha256.Sum256(body)
		manifest.Entries = append(manifest.Entries, Entry{
			URI: "qmd://" + collection + "/" + relative, RelativePath: relative,
			VaultUID: source.VaultUID, NodeID: source.NodeID,
			ContentVersionID: source.ContentVersionID, ProcessingProfileFingerprint: source.ProcessingProfileFingerprint,
			AttachmentID: source.AttachmentID, BuildID: source.BuildID, ArtifactID: source.ArtifactID,
			BlobSHA256: source.BlobSHA256, BlobSize: source.BlobSize,
			ArtifactChecksum: source.ArtifactChecksum, MarkdownChecksum: source.MarkdownChecksum,
			ExportedMarkdownSHA256: hex.EncodeToString(exportedDigest[:]), Frontmatter: frontmatter,
		})
	}
	slices.SortFunc(manifest.Entries, func(a, b Entry) int { return strings.Compare(a.URI, b.URI) })
	checksum, err := manifestChecksum(manifest)
	if err != nil {
		return Generation{}, err
	}
	manifest.Checksum = checksum
	encodedManifest, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil || int64(len(encodedManifest)+1) > bounds.MaxManifestBytes {
		return Generation{}, errors.New("qmd export manifest exceeds bound")
	}
	return Generation{ID: checksum, Manifest: manifest, documents: documents}, nil
}

// Publish builds, stages, and selects one complete immutable generation, then
// removes older disposable generations after CURRENT points at the new one.
func Publish(ctx context.Context, root, collection string, sources []Source, reader BlobReader, options Options) (Receipt, error) {
	return publish(ctx, root, collection, sources, reader, options, publishHooks{})
}

func publish(ctx context.Context, root, collection string, sources []Source, reader BlobReader, options Options, hooks publishHooks) (_ Receipt, retErr error) {
	generation, err := Build(ctx, collection, sources, reader, options)
	if err != nil {
		return Receipt{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil || filesystemRoot(root) {
		return Receipt{}, errors.New("qmd export root is invalid")
	}
	generations := filepath.Join(root, "generations")
	if err := os.MkdirAll(generations, 0o700); err != nil {
		return Receipt{}, fmt.Errorf("create qmd export root: %w", err)
	}
	release, err := acquirePublishLock(ctx, root, hooks.waitingOnLock)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	final := filepath.Join(generations, generation.ID)
	manifestBytes, err := json.Marshal(generation.Manifest, json.Deterministic(true))
	if err != nil {
		return Receipt{}, fmt.Errorf("encode qmd export manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if _, statErr := os.Stat(final); errors.Is(statErr, os.ErrNotExist) {
		stage, err := os.MkdirTemp(generations, ".stage-")
		if err != nil {
			return Receipt{}, fmt.Errorf("stage qmd export generation: %w", err)
		}
		published := false
		defer func() {
			if !published {
				_ = os.RemoveAll(stage)
			}
		}()
		if err := os.MkdirAll(filepath.Join(stage, "collection"), 0o700); err != nil {
			return Receipt{}, fmt.Errorf("stage qmd export collection: %w", err)
		}
		for relative, content := range generation.documents {
			path := filepath.Join(stage, "collection", filepath.FromSlash(relative))
			if err := writePrivateFile(path, content); err != nil {
				return Receipt{}, err
			}
		}
		if err := writePrivateFile(filepath.Join(stage, "manifest.json"), manifestBytes); err != nil {
			return Receipt{}, err
		}
		if err := os.Rename(stage, final); err != nil {
			return Receipt{}, fmt.Errorf("publish qmd export generation: %w", err)
		}
		published = true
	} else if statErr != nil {
		return Receipt{}, fmt.Errorf("inspect qmd export generation: %w", statErr)
	}
	if err := verifyGeneration(final, generation, manifestBytes); err != nil {
		return Receipt{}, err
	}
	if err := publishCurrent(root, generation.ID); err != nil {
		return Receipt{}, err
	}
	if hooks.afterCurrent != nil {
		hooks.afterCurrent()
	}
	if err := removeStaleGenerations(generations, generation.ID); err != nil {
		return Receipt{}, err
	}
	return Receipt{GenerationID: generation.ID, CollectionPath: filepath.Join(final, "collection"), Manifest: generation.Manifest}, nil
}

func readSource(ctx context.Context, reader BlobReader, source Source) (ret []byte, retErr error) {
	stream, size, err := reader.OpenStreamContext(ctx, source.BlobSHA256)
	if err != nil {
		return nil, fmt.Errorf("open retained Markdown: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if size != source.BlobSize {
		return nil, errors.New("retained Markdown size does not match catalog")
	}
	content, err := io.ReadAll(io.LimitReader(stream, source.BlobSize+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("read retained Markdown")
	}
	if int64(len(content)) != source.BlobSize {
		return nil, errors.New("retained Markdown read size does not match catalog")
	}
	if err := stream.Verify(); err != nil {
		return nil, fmt.Errorf("verify retained Markdown: %w", err)
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	if actual != source.BlobSHA256 || actual != source.MarkdownChecksum {
		return nil, errors.New("retained Markdown checksum does not match catalog")
	}
	if !utf8.Valid(content) {
		return nil, errors.New("retained Markdown is not valid UTF-8")
	}
	return content, nil
}

func splitFrontmatter(markdown []byte) (string, []byte) {
	if !bytes.HasPrefix(markdown, []byte("---\n")) {
		return "", slices.Clone(markdown)
	}
	rest := markdown[4:]
	end := -1
	delimiterSize := 0
	for _, delimiter := range [][]byte{[]byte("\n---\n"), []byte("\n...\n")} {
		if index := bytes.Index(rest, delimiter); index >= 0 && (end < 0 || index < end) {
			end = index
			delimiterSize = len(delimiter)
		}
	}
	if end >= 0 {
		return string(rest[:end]), slices.Clone(rest[end+delimiterSize:])
	}
	return "", slices.Clone(markdown)
}

func sourcePathIdentity(source Source) string {
	value := strings.Join([]string{source.VaultUID, strconv.FormatInt(source.NodeID, 10), source.ContentVersionID,
		source.ProcessingProfileFingerprint, source.AttachmentID, source.BuildID, source.ArtifactID,
		source.BlobSHA256, source.ArtifactChecksum}, "\x00")
	digest := sha256.Sum256([]byte(ManifestFormatV1 + "\x00path\x00" + value))
	return hex.EncodeToString(digest[:])
}

func manifestChecksum(manifest Manifest) (string, error) {
	manifest.Checksum = ""
	encoded, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode qmd export identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateSource(source Source) error {
	for _, value := range []string{source.VaultUID, source.ContentVersionID, source.AttachmentID, source.ArtifactID} {
		if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("source identity is invalid")
		}
	}
	for _, value := range []string{source.ProcessingProfileFingerprint, source.BuildID, source.BlobSHA256,
		source.ArtifactChecksum, source.MarkdownChecksum} {
		if !validChecksum(value) {
			return errors.New("source checksum identity is invalid")
		}
	}
	if source.NodeID < 1 || source.BlobSize < 0 || source.BlobSHA256 != source.MarkdownChecksum ||
		source.BlobSHA256 != source.ArtifactChecksum {
		return errors.New("source authority is inconsistent")
	}
	return nil
}

func validChecksum(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validCollection(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func normalizeOptions(options Options) (Options, error) {
	if options.MaxDocuments == 0 {
		options.MaxDocuments = defaultMaxDocuments
	}
	if options.MaxDocumentBytes == 0 {
		options.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = defaultMaxTotalBytes
	}
	if options.MaxManifestBytes == 0 {
		options.MaxManifestBytes = maxManifestBytes
	}
	if options.MaxDocuments < 1 || options.MaxDocuments > defaultMaxDocuments ||
		options.MaxDocumentBytes < 1 || options.MaxDocumentBytes > defaultMaxDocumentBytes ||
		options.MaxTotalBytes < 1 || options.MaxTotalBytes > defaultMaxTotalBytes ||
		options.MaxManifestBytes < 1 || options.MaxManifestBytes > maxManifestBytes {
		return Options{}, errors.New("qmd export bounds are invalid")
	}
	return options, nil
}

func filesystemRoot(value string) bool {
	volume := filepath.VolumeName(value)
	return filepath.Clean(value) == filepath.Clean(volume+string(filepath.Separator))
}

func compareSource(a, b Source) int {
	left := sourcePathIdentity(a)
	right := sourcePathIdentity(b)
	return strings.Compare(left, right)
}

func writePrivateFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create qmd export directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create qmd export file: %w", err)
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write qmd export file: %w", err)
	}
	return nil
}

func acquirePublishLock(ctx context.Context, root string, waiting func()) (func() error, error) {
	lock := flock.New(filepath.Join(root, ".publish.lock"), flock.SetPermissions(0o600))
	for {
		locked, err := lock.TryLock()
		if err != nil {
			return nil, fmt.Errorf("acquire qmd export publication lock: %w", err)
		}
		if locked {
			return lock.Unlock, nil
		}
		if waiting != nil {
			waiting()
			waiting = nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire qmd export publication lock: %w", ctx.Err())
		case <-time.After(publishLockRetry):
		}
	}
}

func verifyGeneration(root string, generation Generation, manifestBytes []byte) error {
	if err := verifyDirectory(root); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	collection := filepath.Join(root, "collection")
	if err := verifyDirectory(collection); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	if err := verifyFile(filepath.Join(root, "manifest.json"), manifestBytes); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	expected := make(map[string][]byte, len(generation.documents))
	for relative, content := range generation.documents {
		expected[filepath.Clean(filepath.FromSlash(relative))] = content
	}
	err := filepath.WalkDir(collection, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == collection {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("collection contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(collection, path)
		if err != nil {
			return err
		}
		content, exists := expected[relative]
		if !exists {
			return errors.New("collection contains an unexpected file")
		}
		if err := verifyFile(path, content); err != nil {
			return err
		}
		delete(expected, relative)
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	if len(expected) != 0 {
		return errors.New("verify qmd export generation: collection is incomplete")
	}
	return nil
}

func verifyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("expected a real directory")
	}
	return nil
}

func verifyFile(path string, expected []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
		return errors.New("expected a regular file with exact size")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, expected) {
		return errors.New("file bytes do not match generation identity")
	}
	return nil
}

func publishCurrent(root, generationID string) (retErr error) {
	temporary, err := os.CreateTemp(root, ".current-")
	if err != nil {
		return fmt.Errorf("stage qmd export current pointer: %w", err)
	}
	path := temporary.Name()
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.Remove(path))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	_, writeErr := io.WriteString(temporary, generationID+"\n")
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write qmd export current pointer: %w", err)
	}
	if err := atomicReplace(path, filepath.Join(root, "CURRENT")); err != nil {
		return fmt.Errorf("publish qmd export current pointer: %w", err)
	}
	return nil
}

func removeStaleGenerations(root, current string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list qmd export generations: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == current {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale qmd export generation: %w", err)
		}
	}
	return nil
}
