package qmdexport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	jsontext "encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

var errManifestBound = errors.New("qmd export manifest exceeds bound")

type manifestBudgetWriter struct {
	destination io.Writer
	remaining   int64
}

func (writer *manifestBudgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > writer.remaining {
		return 0, errManifestBound
	}
	n, err := writer.destination.Write(p)
	writer.remaining -= int64(n)
	return n, err
}

func manifestChecksum(manifest Manifest, maximum int64) (string, error) {
	manifest.Checksum = ""
	encoded, err := encodeManifestJSON(manifest, maximum)
	if err != nil {
		return "", fmt.Errorf("encode qmd export identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func encodeManifest(manifest Manifest, maximum int64) ([]byte, error) {
	if maximum < 1 {
		return nil, errManifestBound
	}
	var buffer bytes.Buffer
	writer := manifestBudgetWriter{destination: &buffer, remaining: maximum}
	if err := json.MarshalWrite(&writer, manifest, json.Deterministic(true)); err != nil {
		return nil, fmt.Errorf("encode qmd export manifest: %w", err)
	}
	if _, err := writer.Write([]byte{'\n'}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeManifestJSON(manifest Manifest, maximum int64) ([]byte, error) {
	if maximum < 0 {
		return nil, errManifestBound
	}
	var buffer bytes.Buffer
	writer := manifestBudgetWriter{destination: &buffer, remaining: maximum}
	if err := json.MarshalWrite(&writer, manifest, json.Deterministic(true)); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func decodeManifest(encoded []byte, generationID string, bounds Options) (Manifest, error) {
	if int64(len(encoded)) > bounds.MaxManifestBytes || len(encoded) == 0 || encoded[len(encoded)-1] != '\n' ||
		!utf8.Valid(encoded) {
		return Manifest{}, errors.New("qmd export manifest encoding is invalid")
	}
	var manifest Manifest
	unmarshalEntries := json.UnmarshalFromFunc(func(decoder *jsontext.Decoder, entries *[]Entry) error {
		opening, err := decoder.ReadToken()
		if err != nil || opening.Kind() != '[' {
			return errors.New("qmd export manifest entries are invalid")
		}
		result := make([]Entry, 0, min(bounds.MaxDocuments, 128))
		for decoder.PeekKind() != ']' {
			if len(result) == bounds.MaxDocuments {
				return errMembershipBound
			}
			var entry Entry
			if err := json.UnmarshalDecode(decoder, &entry, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			result = append(result, entry)
		}
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		*entries = result
		return nil
	})
	if err := json.Unmarshal(encoded, &manifest, json.RejectUnknownMembers(true),
		json.WithUnmarshalers(unmarshalEntries)); err != nil {
		return Manifest{}, errors.New("qmd export manifest encoding is invalid")
	}
	if err := validateManifest(manifest, generationID, bounds); err != nil {
		return Manifest{}, err
	}
	canonical, err := encodeManifest(manifest, bounds.MaxManifestBytes)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return Manifest{}, errors.New("qmd export manifest encoding is not canonical")
	}
	return manifest, nil
}

func validateManifest(manifest Manifest, generationID string, bounds Options) error {
	if manifest.Format != ManifestFormatV1 || !validCollection(manifest.Collection) ||
		manifest.Checksum != generationID || !validChecksum(generationID) ||
		len(manifest.Entries) > bounds.MaxDocuments {
		return errors.New("qmd export manifest identity is invalid")
	}
	checksum, err := manifestChecksum(manifest, bounds.MaxManifestBytes)
	if err != nil || checksum != generationID {
		return errors.New("qmd export manifest identity is invalid")
	}
	if _, err := encodeManifest(manifest, bounds.MaxManifestBytes); err != nil {
		return err
	}
	if !slices.IsSortedFunc(manifest.Entries, func(a, b Entry) int { return strings.Compare(a.URI, b.URI) }) {
		return errors.New("qmd export manifest entries are not canonical")
	}
	var total int64
	for index, entry := range manifest.Entries {
		source := Source{VaultUID: entry.VaultUID, NodeID: entry.NodeID,
			ContentVersionID: entry.ContentVersionID, ProcessingProfileFingerprint: entry.ProcessingProfileFingerprint,
			AttachmentID: entry.AttachmentID, BuildID: entry.BuildID, ArtifactID: entry.ArtifactID,
			BlobSHA256: entry.BlobSHA256, BlobSize: entry.BlobSize,
			ArtifactChecksum: entry.ArtifactChecksum, MarkdownChecksum: entry.MarkdownChecksum}
		if err := validateSource(source); err != nil {
			return errors.New("qmd export manifest entry identity is invalid")
		}
		identity := sourcePathIdentity(source)
		relative := "documents/" + identity[:2] + "/" + identity + ".md"
		if source.BlobSize > bounds.MaxDocumentBytes ||
			source.BlobSize > bounds.MaxTotalBytes-total || !validChecksum(entry.ExportedMarkdownSHA256) ||
			entry.RelativePath != relative || entry.URI != "qmd://"+manifest.Collection+"/"+relative ||
			len(entry.Frontmatter) > maxClaimedFrontmatterBytes || !utf8.ValidString(entry.Frontmatter) ||
			strings.ContainsRune(entry.Frontmatter, 0) {
			return errors.New("qmd export manifest entry identity is invalid")
		}
		total += source.BlobSize
		if index > 0 && manifest.Entries[index-1].URI == entry.URI {
			return errors.New("qmd export manifest contains duplicate entries")
		}
	}
	return nil
}
