package qmdexport

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	json "encoding/json/v2"
)

const maxManifestBytes = int64(256 << 20)

// LoadCurrent reads and validates the exact manifest selected by CURRENT.
func LoadCurrent(root string) (Receipt, error) {
	absolute, err := filepath.Abs(root)
	if err != nil || filesystemRoot(absolute) {
		return Receipt{}, errors.New("qmd export root is invalid")
	}
	current, err := readRegularBounded(filepath.Join(absolute, "CURRENT"), sha256.Size*2+1)
	if err != nil || len(current) != sha256.Size*2+1 || current[len(current)-1] != '\n' {
		return Receipt{}, errors.New("qmd export CURRENT pointer is invalid")
	}
	generationID := string(current[:len(current)-1])
	if !validChecksum(generationID) {
		return Receipt{}, errors.New("qmd export CURRENT pointer is invalid")
	}
	generationRoot := filepath.Join(absolute, "generations", generationID)
	if err := verifyDirectory(generationRoot); err != nil {
		return Receipt{}, fmt.Errorf("load qmd export generation: %w", err)
	}
	collectionPath := filepath.Join(generationRoot, "collection")
	if err := verifyDirectory(collectionPath); err != nil {
		return Receipt{}, fmt.Errorf("load qmd export collection: %w", err)
	}
	encoded, err := readRegularBounded(filepath.Join(generationRoot, "manifest.json"), maxManifestBytes)
	if err != nil {
		return Receipt{}, fmt.Errorf("load qmd export manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest, json.RejectUnknownMembers(true)); err != nil {
		return Receipt{}, errors.New("qmd export manifest encoding is invalid")
	}
	if err := validateManifest(manifest, generationID); err != nil {
		return Receipt{}, err
	}
	return Receipt{GenerationID: generationID, CollectionPath: collectionPath, Manifest: manifest}, nil
}

func validateManifest(manifest Manifest, generationID string) error {
	if manifest.Format != ManifestFormatV1 || !validCollection(manifest.Collection) ||
		manifest.Checksum != generationID || len(manifest.Entries) > defaultMaxDocuments {
		return errors.New("qmd export manifest identity is invalid")
	}
	checksum, err := manifestChecksum(manifest)
	if err != nil || checksum != generationID {
		return errors.New("qmd export manifest identity is invalid")
	}
	if !slices.IsSortedFunc(manifest.Entries, func(a, b Entry) int { return strings.Compare(a.URI, b.URI) }) {
		return errors.New("qmd export manifest entries are not canonical")
	}
	for index, entry := range manifest.Entries {
		source := Source{VaultUID: entry.VaultUID, NodeID: entry.NodeID,
			ContentVersionID: entry.ContentVersionID, ProcessingProfileFingerprint: entry.ProcessingProfileFingerprint,
			AttachmentID: entry.AttachmentID, BuildID: entry.BuildID, ArtifactID: entry.ArtifactID,
			BlobSHA256: entry.BlobSHA256, BlobSize: entry.BlobSize,
			ArtifactChecksum: entry.ArtifactChecksum, MarkdownChecksum: entry.MarkdownChecksum}
		identity := sourcePathIdentity(source)
		relative := "documents/" + identity[:2] + "/" + identity + ".md"
		if err := validateSource(source); err != nil || !validChecksum(entry.ExportedMarkdownSHA256) ||
			entry.RelativePath != relative || entry.URI != "qmd://"+manifest.Collection+"/"+relative ||
			!utf8.ValidString(entry.Frontmatter) || strings.ContainsRune(entry.Frontmatter, 0) {
			return errors.New("qmd export manifest entry identity is invalid")
		}
		if index > 0 && manifest.Entries[index-1].URI == entry.URI {
			return errors.New("qmd export manifest contains duplicate entries")
		}
	}
	return nil
}

func readRegularBounded(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("expected a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) != info.Size() {
		return nil, errors.New("bounded file read failed")
	}
	return content, nil
}
