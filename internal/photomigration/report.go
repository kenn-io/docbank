// Package photomigration contains the durable contracts used by the photo
// migration operator workflow.
package photomigration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	SourceInstall    = "install"
	SourceArchive    = "archive"
	MaxReportBytes   = 4 << 20
	MaxOwnerMapBytes = 4 << 20
)

type Source struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
}

type Schema struct {
	CatalogVersion         int64  `json:"catalog_version,omitzero"`
	CatalogFingerprint     string `json:"catalog_fingerprint,omitzero"`
	EmbeddedDocbankVersion int64  `json:"embedded_docbank_version,omitzero"`
	ArchiveMetadataFormat  string `json:"archive_metadata_format,omitzero"`
}

type Counts struct {
	Owners           int64 `json:"owners"`
	Assets           int64 `json:"assets"`
	Files            int64 `json:"files"`
	Bytes            int64 `json:"bytes"`
	Albums           int64 `json:"albums"`
	AlbumMemberships int64 `json:"album_memberships"`
	Shares           int64 `json:"shares"`
	Checkouts        int64 `json:"checkouts"`
	CheckoutEntries  int64 `json:"checkout_entries"`
	AIResults        int64 `json:"ai_results"`
	HiddenSetup      int64 `json:"hidden_setup"`
}

type VectorGeneration struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Rebuildable bool   `json:"rebuildable"`
}

type Capacity struct {
	SourceBytes         int64 `json:"source_bytes"`
	UniqueBlobBytes     int64 `json:"unique_blob_bytes"`
	MinimumContentBytes int64 `json:"minimum_content_bytes"`
}

// Report is the complete, portable result of one inventory read. It contains
// stable identities and counts only. Host paths, secrets and source text stay
// outside the durable contract.
type Report struct {
	Source    Source             `json:"source"`
	Schema    Schema             `json:"schema"`
	Counts    Counts             `json:"counts"`
	Vectors   []VectorGeneration `json:"vectors,omitzero"`
	Capacity  Capacity           `json:"capacity"`
	CreatedAt string             `json:"created_at"`
}

type MapEntry struct {
	SourceHub      string `json:"source_hub"`
	SourceUserID   string `json:"source_user_id"`
	StorageKey     string `json:"storage_key"`
	DocbankOwnerID string `json:"docbank_owner_id,omitzero"`
}

type OwnerMapTemplate struct {
	Source  Source     `json:"source"`
	Entries []MapEntry `json:"entries"`
}

func EncodeReport(report Report) ([]byte, error) {
	if err := ValidateReport(report); err != nil {
		return nil, err
	}
	raw, err := canonical.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("encode migration report: %w", err)
	}
	if len(raw) > MaxReportBytes {
		return nil, fmt.Errorf("migration report exceeds %d bytes", MaxReportBytes)
	}
	return raw, nil
}

func DecodeReport(raw []byte) (Report, error) {
	if len(raw) == 0 || len(raw) > MaxReportBytes {
		return Report{}, errors.New("migration report has invalid size")
	}
	report, err := canonical.Decode[Report](raw)
	if err != nil {
		return Report{}, fmt.Errorf("decode migration report: %w", err)
	}
	if err := ValidateReport(report); err != nil {
		return Report{}, err
	}
	return report, nil
}

func ValidateReport(report Report) error {
	if report.Source.Kind != SourceInstall && report.Source.Kind != SourceArchive {
		return fmt.Errorf("migration report has unsupported source kind %q", report.Source.Kind)
	}
	if report.Source.Identity == "" || strings.ContainsAny(report.Source.Identity, "/\\:\x00\r\n") {
		return errors.New("migration report has invalid source identity")
	}
	if report.CreatedAt == "" {
		return errors.New("migration report created_at is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, report.CreatedAt); err != nil {
		return fmt.Errorf("migration report has invalid created_at: %w", err)
	}
	if report.Schema.CatalogVersion < 0 || report.Schema.EmbeddedDocbankVersion < 0 {
		return errors.New("migration report has negative schema version")
	}
	if report.Counts.Owners < 0 || report.Counts.Assets < 0 || report.Counts.Files < 0 ||
		report.Counts.Bytes < 0 || report.Counts.Albums < 0 || report.Counts.Shares < 0 ||
		report.Counts.AlbumMemberships < 0 || report.Counts.Checkouts < 0 || report.Counts.CheckoutEntries < 0 ||
		report.Counts.AIResults < 0 || report.Counts.HiddenSetup < 0 {
		return errors.New("migration report has negative count")
	}
	if report.Capacity.SourceBytes < 0 || report.Capacity.UniqueBlobBytes < 0 ||
		report.Capacity.MinimumContentBytes < 0 {
		return errors.New("migration report has negative capacity")
	}
	seen := make(map[int64]struct{}, len(report.Vectors))
	for _, vector := range report.Vectors {
		if vector.ID < 1 || vector.Fingerprint == "" || vector.State == "" || !vector.Rebuildable {
			return errors.New("migration report has invalid vector generation")
		}
		if _, ok := seen[vector.ID]; ok {
			return errors.New("migration report repeats a vector generation")
		}
		seen[vector.ID] = struct{}{}
	}
	return nil
}

func NewOwnerMapTemplate(report Report, entries []MapEntry) (OwnerMapTemplate, error) {
	if err := ValidateReport(report); err != nil {
		return OwnerMapTemplate{}, err
	}
	template := OwnerMapTemplate{Source: report.Source, Entries: slices.Clone(entries)}
	for i := range template.Entries {
		if err := ValidateMapEntry(template.Entries[i]); err != nil {
			return OwnerMapTemplate{}, err
		}
	}
	sort.Slice(template.Entries, func(i, j int) bool {
		left, right := template.Entries[i], template.Entries[j]
		if left.SourceHub != right.SourceHub {
			return left.SourceHub < right.SourceHub
		}
		if left.SourceUserID != right.SourceUserID {
			return left.SourceUserID < right.SourceUserID
		}
		return left.StorageKey < right.StorageKey
	})
	return template, nil
}

func ValidateOwnerMapTemplate(template OwnerMapTemplate) error {
	if template.Source.Kind != SourceInstall && template.Source.Kind != SourceArchive {
		return errors.New("owner map template has unsupported source kind")
	}
	if template.Source.Identity == "" || strings.ContainsAny(template.Source.Identity, "/\\:\x00\r\n") {
		return errors.New("owner map template has no source identity")
	}
	previous := ""
	for _, entry := range template.Entries {
		if err := ValidateMapEntry(entry); err != nil {
			return err
		}
		key := entry.SourceHub + "\x00" + entry.SourceUserID + "\x00" + entry.StorageKey
		if key <= previous && previous != "" {
			return errors.New("owner map entries are not sorted")
		}
		previous = key
	}
	return nil
}

func ValidateMapEntry(entry MapEntry) error {
	if entry.SourceHub == "" || entry.SourceUserID == "" || entry.StorageKey == "" {
		return errors.New("owner map entry requires source hub, user ID and storage key")
	}
	if strings.ContainsAny(entry.SourceHub+entry.SourceUserID+entry.StorageKey+entry.DocbankOwnerID, "\\\x00\r\n") {
		return errors.New("owner map entry contains an invalid character")
	}
	return nil
}

func WriteOwnerMapTemplate(path string, template OwnerMapTemplate, sourceRoots ...string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("owner map path must be absolute")
	}
	if len(template.Entries) > MaxReportBytes {
		return errors.New("owner map has too many entries")
	}
	for _, entry := range template.Entries {
		if err := ValidateMapEntry(entry); err != nil {
			return err
		}
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, root := range sourceRoots {
		if root == "" {
			continue
		}
		within, err := pathWithin(cleanPath, root)
		if err != nil {
			return fmt.Errorf("resolve owner map source boundary: %w", err)
		}
		if within {
			return errors.New("owner map path overlaps a source tree")
		}
	}
	raw, err := canonical.Marshal(template)
	if err != nil {
		return fmt.Errorf("encode owner map template: %w", err)
	}
	if len(raw) > MaxOwnerMapBytes {
		return fmt.Errorf("owner map template exceeds %d bytes", MaxOwnerMapBytes)
	}
	f, err := os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create owner map template: %w", err)
	}
	if _, writeErr := f.Write(append(raw, '\n')); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(cleanPath)
		return fmt.Errorf("write owner map template: %w", writeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(cleanPath)
		return fmt.Errorf("close owner map template: %w", closeErr)
	}
	return nil
}

func EncodeOwnerMapTemplate(template OwnerMapTemplate) ([]byte, error) {
	if err := ValidateOwnerMapTemplate(template); err != nil {
		return nil, err
	}
	raw, err := canonical.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("encode owner map template: %w", err)
	}
	if len(raw) > MaxOwnerMapBytes {
		return nil, fmt.Errorf("owner map template exceeds %d bytes", MaxOwnerMapBytes)
	}
	return raw, nil
}

func DecodeOwnerMapTemplate(raw []byte) (OwnerMapTemplate, error) {
	if len(raw) == 0 || len(raw) > MaxOwnerMapBytes+1 {
		return OwnerMapTemplate{}, errors.New("owner map template has invalid size")
	}
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	var template OwnerMapTemplate
	decoded, err := canonical.Decode[OwnerMapTemplate](raw)
	if err != nil {
		return template, err
	}
	if decoded.Source.Kind != SourceInstall && decoded.Source.Kind != SourceArchive || decoded.Source.Identity == "" {
		return template, errors.New("owner map template has invalid source")
	}
	if err := ValidateOwnerMapTemplate(decoded); err != nil {
		return template, err
	}
	return decoded, nil
}

func pathWithin(path, root string) (bool, error) {
	path, err := resolveExistingPath(path)
	if err != nil {
		return false, err
	}
	root, err = resolveExistingPath(root)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(filepath.VolumeName(path), filepath.VolumeName(root)) {
		return false, nil
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".", err
}

// Resolving the nearest existing prefix follows aliases before an output leaf exists.
func resolveExistingPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	existing := path
	var missing []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing path prefix for %q", path)
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	existing, err = filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := range slices.Backward(missing) {
		existing = filepath.Join(existing, missing[i])
	}
	return filepath.Clean(existing), nil
}
