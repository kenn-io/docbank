package qmdexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	errMembershipBound  = errors.New("qmd export document membership exceeds bound")
	errSourceBytesBound = errors.New("qmd export source bytes exceed bound")
)

// Build reads and verifies every exact source before returning a generation.
func Build(
	ctx context.Context, collection string, sources []Source, reader BlobReader, options Options,
) (Generation, error) {
	if ctx == nil {
		return Generation{}, errors.New("qmd export requires context")
	}
	if err := ctx.Err(); err != nil {
		return Generation{}, err
	}
	if reader == nil {
		return Generation{}, errors.New("qmd export requires a blob reader")
	}
	if !validCollection(collection) {
		return Generation{}, errors.New("qmd export collection name is invalid")
	}
	bounds, err := normalizeOptions(options)
	if err != nil {
		return Generation{}, err
	}
	if len(sources) > bounds.MaxDocuments {
		return Generation{}, errMembershipBound
	}
	canonical := slices.Clone(sources)
	var total int64
	for index, source := range canonical {
		if err := validateSource(source); err != nil {
			return Generation{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
		if source.BlobSize > bounds.MaxDocumentBytes || source.BlobSize > bounds.MaxTotalBytes-total {
			return Generation{}, errSourceBytesBound
		}
		total += source.BlobSize
	}
	slices.SortFunc(canonical, compareSource)
	for index := 1; index < len(canonical); index++ {
		if compareSource(canonical[index-1], canonical[index]) == 0 {
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
		frontmatter, body, err := splitFrontmatter(markdown)
		if err != nil {
			return Generation{}, fmt.Errorf("qmd export source %d: %w", index, err)
		}
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
	checksum, err := manifestChecksum(manifest, bounds.MaxManifestBytes)
	if err != nil {
		return Generation{}, err
	}
	manifest.Checksum = checksum
	if _, err := encodeManifest(manifest, bounds.MaxManifestBytes); err != nil {
		return Generation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Generation{}, err
	}
	return Generation{ID: checksum, Manifest: manifest, documents: documents}, nil
}

func sourcePathIdentity(source Source) string {
	value := strings.Join([]string{source.VaultUID, strconv.FormatInt(source.NodeID, 10), source.ContentVersionID,
		source.ProcessingProfileFingerprint, source.AttachmentID, source.BuildID, source.ArtifactID,
		source.BlobSHA256, source.ArtifactChecksum}, "\x00")
	digest := sha256.Sum256([]byte(ManifestFormatV1 + "\x00path\x00" + value))
	return hex.EncodeToString(digest[:])
}

func compareSource(a, b Source) int {
	return strings.Compare(sourcePathIdentity(a), sourcePathIdentity(b))
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
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validCollection(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
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
