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
	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/safefileio"
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

// PublishActive holds the publication lock while snapshotting the catalog and
// publishing its complete current set, so concurrent exports cannot reorder snapshots.
func PublishActive(ctx context.Context, root, collection string, catalog SourceCatalog, reader BlobReader, options Options) (Receipt, error) {
	if catalog == nil {
		return Receipt{}, errors.New("qmd export requires a source catalog")
	}
	return publish(ctx, root, collection, nil, catalog, reader, options, publishHooks{})
}

// build stages verified source streams and returns their complete manifest.
func build(ctx context.Context, stage, collection string, sources []Source, reader BlobReader) (Manifest, error) {
	manifest := Manifest{Format: ManifestFormatV1, Collection: collection}
	for index, source := range sources {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		identity := sourcePathIdentity(source)
		relative := "documents/" + identity[:2] + "/" + identity + ".md"
		if err := writeSource(ctx, filepath.Join(stage, "collection", filepath.FromSlash(relative)), reader, source); err != nil {
			return Manifest{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
		manifest.Entries = append(manifest.Entries, Entry{
			URI: "qmd://" + collection + "/" + relative, RelativePath: relative,
			VaultUID: source.VaultUID, NodeID: source.NodeID,
			ContentVersionID: source.ContentVersionID, ProcessingProfileFingerprint: source.ProcessingProfileFingerprint,
			AttachmentID: source.AttachmentID, BuildID: source.BuildID, ArtifactID: source.ArtifactID,
			BlobSHA256: source.BlobSHA256, BlobSize: source.BlobSize,
			ArtifactChecksum: source.ArtifactChecksum, MarkdownChecksum: source.MarkdownChecksum,
			ExportedMarkdownSHA256: source.BlobSHA256,
		})
	}
	checksum, err := manifestChecksum(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Checksum = checksum
	return manifest, nil
}

// Publish stages verified streams and selects one complete immutable generation.
// Retired generations remain readable. The caller may remove them only when no
// consumer uses them; publication cleans up abandoned staging directories only.
func Publish(ctx context.Context, root, collection string, sources []Source, reader BlobReader, options Options) (Receipt, error) {
	return publish(ctx, root, collection, sources, nil, reader, options, publishHooks{})
}

func publish(ctx context.Context, root, collection string, sources []Source, catalog SourceCatalog, reader BlobReader, options Options, hooks publishHooks) (_ Receipt, retErr error) {
	if ctx == nil || reader == nil {
		return Receipt{}, errors.New("qmd export requires context and blob reader")
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if !validCollection(collection) {
		return Receipt{}, errors.New("qmd export collection name is invalid")
	}
	bounds, err := normalizeOptions(options)
	if err != nil {
		return Receipt{}, err
	}

	root, err = filepath.Abs(root)
	if err != nil {
		return Receipt{}, fmt.Errorf("resolve qmd export root: %w", err)
	}
	if filepath.Dir(root) == root {
		return Receipt{}, errors.New("qmd export root is invalid")
	}
	generations := filepath.Join(root, "generations")
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := preparePrivateDir(root); err != nil {
		return Receipt{}, fmt.Errorf("prepare qmd export root: %w", err)
	}
	if err := preparePrivateDir(generations); err != nil {
		return Receipt{}, fmt.Errorf("create qmd export root: %w", err)
	}
	release, err := acquirePublishLock(ctx, root, hooks.waitingOnLock)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	if catalog != nil {
		sources, err = catalog.QMDExportSources(ctx, bounds.MaxDocuments)
		if err != nil {
			return Receipt{}, fmt.Errorf("snapshot qmd export sources: %w", err)
		}
	}
	if len(sources) > bounds.MaxDocuments {
		return Receipt{}, errors.New("qmd export document membership exceeds bound")
	}
	canonical := slices.Clone(sources)
	slices.SortFunc(canonical, compareSource)
	var total int64
	for index, source := range canonical {
		if err := validateSource(source); err != nil {
			return Receipt{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
		if source.BlobSize > bounds.MaxDocumentBytes || source.BlobSize > bounds.MaxTotalBytes-total {
			return Receipt{}, errors.New("qmd export source bytes exceed bound")
		}
		total += source.BlobSize
		if index > 0 && compareSource(canonical[index-1], source) == 0 {
			return Receipt{}, errors.New("qmd export contains duplicate source authority")
		}
	}
	if err := validateExistingPrivateFile(filepath.Join(root, "CURRENT")); err != nil {
		return Receipt{}, fmt.Errorf("inspect qmd export current pointer: %w", err)
	}
	if err := removeAbandonedStages(generations); err != nil {
		return Receipt{}, err
	}
	stage, err := os.MkdirTemp(generations, ".stage-")
	if err != nil {
		return Receipt{}, fmt.Errorf("stage qmd export generation: %w", err)
	}
	defer func() {
		if stage != "" {
			retErr = errors.Join(retErr, os.RemoveAll(stage))
		}
	}()
	if err := safefileio.EnsurePrivateDir(stage); err != nil {
		return Receipt{}, fmt.Errorf("secure qmd export stage: %w", err)
	}
	if err := preparePrivateDir(filepath.Join(stage, "collection")); err != nil {
		return Receipt{}, fmt.Errorf("stage qmd export collection: %w", err)
	}
	if err := preparePrivateDir(filepath.Join(stage, "collection", "documents")); err != nil {
		return Receipt{}, fmt.Errorf("stage qmd export collection: %w", err)
	}
	manifest, err := build(ctx, stage, collection, canonical, reader)
	if err != nil {
		return Receipt{}, err
	}
	manifestBytes, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return Receipt{}, fmt.Errorf("encode qmd export manifest: %w", err)
	}
	if int64(len(manifestBytes)+1) > bounds.MaxManifestBytes {
		return Receipt{}, errors.New("qmd export manifest exceeds bound")
	}
	manifestBytes = append(manifestBytes, '\n')
	if _, err := writePrivateFile(filepath.Join(stage, "manifest.json"), bytes.NewReader(manifestBytes)); err != nil {
		return Receipt{}, err
	}
	if err := verifyGeneration(stage, manifest, manifestBytes); err != nil {
		return Receipt{}, err
	}
	if err := syncGeneration(stage); err != nil {
		return Receipt{}, err
	}
	final := filepath.Join(generations, manifest.Checksum)
	if _, statErr := os.Stat(final); errors.Is(statErr, os.ErrNotExist) {
		if err := ctx.Err(); err != nil {
			return Receipt{}, err
		}
		if err := renamePublished(stage, final); err != nil {
			return Receipt{}, fmt.Errorf("publish qmd export generation: %w", err)
		}
	} else if statErr != nil {
		return Receipt{}, fmt.Errorf("inspect qmd export generation: %w", statErr)
	} else {
		if err := verifyGeneration(final, manifest, manifestBytes); err != nil {
			return Receipt{}, err
		}
		if err := syncGeneration(final); err != nil {
			return Receipt{}, err
		}
		if err := os.RemoveAll(stage); err != nil {
			return Receipt{}, fmt.Errorf("remove duplicate qmd export stage: %w", err)
		}
	}
	stage = ""
	if err := pack.SyncDir(generations); err != nil {
		return Receipt{}, fmt.Errorf("sync qmd export generations: %w", err)
	}
	if err := publishCurrent(ctx, root, manifest.Checksum); err != nil {
		return Receipt{}, err
	}
	if hooks.afterCurrent != nil {
		hooks.afterCurrent()
	}
	// ponytail: retired generations accumulate; add pruning with consumer ownership.
	return Receipt{GenerationID: manifest.Checksum, CollectionPath: filepath.Join(final, "collection"), Manifest: manifest}, nil
}

func writeSource(ctx context.Context, path string, reader BlobReader, source Source) (retErr error) {
	stream, size, err := reader.OpenStreamContext(ctx, source.BlobSHA256)
	if err != nil {
		return fmt.Errorf("open retained Markdown: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if size != source.BlobSize {
		return errors.New("retained Markdown size does not match catalog")
	}
	digest := sha256.New()
	validated := transform.NewReader(io.LimitReader(stream, source.BlobSize+1), encoding.UTF8Validator)
	written, err := writePrivateFile(path, io.TeeReader(validated, digest))
	if err != nil {
		return fmt.Errorf("copy retained Markdown: %w", errors.Join(err, ctx.Err()))
	}
	if written != source.BlobSize {
		return errors.New("retained Markdown read size does not match catalog")
	}
	if err := stream.Verify(); err != nil {
		return fmt.Errorf("verify retained Markdown: %w", err)
	}
	if hex.EncodeToString(digest.Sum(nil)) != source.BlobSHA256 {
		return errors.New("retained Markdown checksum does not match catalog")
	}
	return nil
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

func compareSource(a, b Source) int {
	left := sourcePathIdentity(a)
	right := sourcePathIdentity(b)
	return strings.Compare(left, right)
}

func writePrivateFile(path string, content io.Reader) (int64, error) {
	if err := preparePrivateDir(filepath.Dir(path)); err != nil {
		return 0, fmt.Errorf("create qmd export directory: %w", err)
	}
	file, err := createPrivateFile(path)
	if err != nil {
		return 0, fmt.Errorf("create qmd export file: %w", err)
	}
	written, writeErr := io.Copy(file, content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return 0, fmt.Errorf("write qmd export file: %w", err)
	}
	return written, nil
}

func acquirePublishLock(ctx context.Context, root string, waiting func()) (func() error, error) {
	path := filepath.Join(root, ".publish.lock")
	file, err := createPrivateFile(path)
	if errors.Is(err, os.ErrExist) {
		file, err = openPrivateFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("prepare qmd export publication lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	lock := flock.New(path, flock.SetPermissions(0o600))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

func verifyGeneration(root string, manifest Manifest, manifestBytes []byte) error {
	if err := verifyDirectory(root); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	collection := filepath.Join(root, "collection")
	if err := verifyDirectory(collection); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	if err := verifyFile(filepath.Join(root, "manifest.json"), int64(len(manifestBytes)), hex.EncodeToString(manifestDigest[:])); err != nil {
		return fmt.Errorf("verify qmd export generation: %w", err)
	}
	expected := make(map[string]Entry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		expected[filepath.Clean(filepath.FromSlash(entry.RelativePath))] = entry
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
			return verifyDirectory(path)
		}
		relative, err := filepath.Rel(collection, path)
		if err != nil {
			return err
		}
		content, exists := expected[relative]
		if !exists {
			return errors.New("collection contains an unexpected file")
		}
		if err := verifyFile(path, content.BlobSize, content.ExportedMarkdownSHA256); err != nil {
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
	if err := safefileio.ValidatePrivateDir(path); err != nil {
		return fmt.Errorf("validate private qmd export directory: %w", err)
	}
	return nil
}

func verifyFile(path string, size int64, checksum string) (retErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("expected a regular file with exact size")
	}
	file, err := openPrivateFile(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, size+1))
	if err != nil {
		return err
	}
	if written != size || hex.EncodeToString(digest.Sum(nil)) != checksum {
		return errors.New("file bytes do not match generation identity")
	}
	return nil
}

func publishCurrent(ctx context.Context, root, generationID string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, ".current-")
	if err != nil {
		return fmt.Errorf("stage qmd export current pointer: %w", err)
	}
	path := temporary.Name()
	defer func() {
		if path != "" {
			retErr = errors.Join(retErr, os.Remove(path))
		}
	}()
	if err := restrictNewFile(path); err != nil {
		return errors.Join(err, temporary.Close())
	}
	_, writeErr := io.WriteString(temporary, generationID+"\n")
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write qmd export current pointer: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renamePublished(path, filepath.Join(root, "CURRENT")); err != nil {
		return fmt.Errorf("publish qmd export current pointer: %w", err)
	}
	path = ""
	if err := pack.SyncDir(root); err != nil {
		return fmt.Errorf("sync qmd export current pointer directory: %w", err)
	}
	return nil
}

func removeAbandonedStages(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list qmd export generations: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".stage-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove abandoned qmd export stage: %w", err)
		}
	}
	return nil
}

// Existing paths must already be private; only newly created directories are secured.
func preparePrivateDir(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return verifyDirectory(path)
	}
	if err := pack.MkdirAllSynced(path); err != nil {
		return fmt.Errorf("create private qmd export directory: %w", err)
	}
	if err := safefileio.EnsurePrivateDir(path); err != nil {
		return fmt.Errorf("secure qmd export directory: %w", err)
	}
	return nil
}

func createPrivateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := restrictNewFile(path); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func validateExistingPrivateFile(path string) error {
	file, err := openPrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}

func syncGeneration(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list qmd export directories: %w", err)
	}
	for _, path := range slices.Backward(directories) {
		if err := pack.SyncDir(path); err != nil {
			return fmt.Errorf("sync qmd export directory: %w", err)
		}
	}
	return nil
}
