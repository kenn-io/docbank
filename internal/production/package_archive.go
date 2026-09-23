package production

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/kit/packstore"
)

const (
	PackageQCContractV1            = "production-package-qc/v1"
	RecipientTransmittalContractV1 = "production-transmittal/v1"
	maxPackageMetadataBytes        = 128 << 20
)

// archive/zip writes this fixed extended timestamp for the 1980 date set by
// writePackageEntry. Other extra fields are outside the public projection.
var packageZIPTimestampExtra = []byte{0x55, 0x54, 5, 0, 1, 0, 0xa6, 0xce, 0x12}

var ErrRecipientArchive = errors.New("recipient archive failed verification")

// PackageArtifactOpener resolves only a job-scoped, manifest-listed artifact.
type PackageArtifactOpener interface {
	OpenVerifiedProductionArtifact(ctx context.Context, jobID string, artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error)
}

// PackageQC is retained outside the immutable recipient archive. A later
// delivery receipt may refer to ArchiveSHA256 without changing the archive.
type PackageQC struct {
	Contract       string           `json:"contract"`
	ArchiveSHA256  string           `json:"archive_sha256"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Entries        []PackageQCEntry `json:"entries"`
	PageNumbers    []string         `json:"page_numbers"`
}

type PackageQCEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type RecipientTransmittal struct {
	Contract  string            `json:"contract"`
	ProfileID string            `json:"profile_id"`
	Volumes   []RecipientVolume `json:"volumes"`
	Documents int               `json:"documents"`
	Pages     int               `json:"pages"`
	FirstPage string            `json:"first_page"`
	LastPage  string            `json:"last_page"`
}

func recipientTransmittal(manifest RecipientManifest) RecipientTransmittal {
	pages := make([]string, 0)
	for _, doc := range manifest.Documents {
		for _, page := range doc.Pages {
			pages = append(pages, page.Number)
		}
	}
	result := RecipientTransmittal{Contract: RecipientTransmittalContractV1,
		ProfileID: manifest.ProfileID, Volumes: slices.Clone(manifest.Volumes),
		Documents: len(manifest.Documents), Pages: len(pages)}
	if len(pages) > 0 {
		result.FirstPage, result.LastPage = pages[0], pages[len(pages)-1]
	}
	return result
}

func packageJSON(value any) ([]byte, error) {
	data, err := canonical.Marshal(value)
	if err != nil || len(data) > maxPackageMetadataBytes {
		return nil, ErrRecipientArchive
	}
	return append(data, '\n'), nil
}

type packageLoadfiles struct {
	dat  []byte
	page []byte
	ext  string
}

func makePackageLoadfiles(manifest RecipientManifest, volume RecipientVolume) (packageLoadfiles, error) {
	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	if err != nil {
		return packageLoadfiles{}, err
	}
	profile.Columns = []string{"BEGDOC", "ENDDOC", "VOLUME", "PAGES", "TEXT", "PDF", "FIRST_IMAGE"}
	records := []loadfile.Record{}
	images := []loadfile.ImageRef{}
	for _, doc := range manifest.Documents {
		if doc.Volume != volume.Name {
			continue
		}
		values := []string{doc.Control, doc.End, volume.Name, strconv.Itoa(len(doc.Pages)),
			doc.TextPath, doc.PDFPath, doc.Images[0].Path}
		fields := make([]loadfile.Field, len(values))
		for index, value := range values {
			fields[index] = loadfile.Field{Column: profile.Columns[index], Ordinal: index, Raw: value}
		}
		records = append(records, loadfile.Record{ColumnOrder: slices.Clone(profile.Columns), Fields: fields})
		for index, image := range doc.Images {
			boundary := ""
			if index == 0 {
				boundary = "document"
			}
			images = append(images, loadfile.ImageRef{ImageKey: image.Number, Volume: volume.Name,
				RelPath: image.Path, DocumentBreak: index == 0, PageOrdinal: index + 1,
				DeclaredPageCount: len(doc.Pages), Boundary: boundary})
		}
	}
	var dat bytes.Buffer
	if err := loadfile.WriteDAT(&dat, records, profile); err != nil {
		return packageLoadfiles{}, err
	}
	pageProfileID := "opt-standard-v1"
	ext := "opt"
	if manifest.ProfileID == "export-dat-lfp-images-v1" {
		pageProfileID, ext = "lfp-ipro-v1", "lfp"
	}
	pageProfile, err := loadfile.ReadProfile(pageProfileID)
	if err != nil {
		return packageLoadfiles{}, err
	}
	var page bytes.Buffer
	if ext == "lfp" {
		err = loadfile.WriteLFP(&page, images, pageProfile)
	} else {
		err = loadfile.WriteOPT(&page, images, pageProfile)
	}
	if err != nil {
		return packageLoadfiles{}, err
	}
	return packageLoadfiles{dat: dat.Bytes(), page: page.Bytes(), ext: ext}, nil
}

func packageExpectedPaths(manifest RecipientManifest) (map[string]struct{}, error) {
	if manifest.Contract != RecipientPackageContractV1 || len(manifest.Volumes) == 0 ||
		len(manifest.Volumes) > 999 || len(manifest.Documents) == 0 || len(manifest.Documents) > 100_000 {
		return nil, ErrRecipientArchive
	}
	roles := []string{documentproduction.ArtifactRoleRedactedPDF, documentproduction.ArtifactRoleRedactedText}
	if manifest.ProfileID != "export-dat-pdf-v1" {
		roles = []string{documentproduction.ArtifactRoleRedactedPage, documentproduction.ArtifactRoleRedactedText}
	}
	if _, err := loadfile.PlanProductionExport(manifest.ProfileID, roles, false); err != nil {
		return nil, ErrRecipientArchive
	}
	paths := map[string]struct{}{"MANIFEST.json": {}, "TRANSMITTAL.json": {}}
	volumeIndex := map[string]int{}
	for index, volume := range manifest.Volumes {
		if volume.Name != fmt.Sprintf("VOL%03d", index+1) || volume.Documents < 1 ||
			volume.Pages < 1 || volume.Bytes < 1 || volume.Bytes > 50<<30 {
			return nil, ErrRecipientArchive
		}
		volumeIndex[volume.Name] = index
		paths[volume.Name+"/LOADFILES/PRODUCTION.dat"] = struct{}{}
		ext := "opt"
		if manifest.ProfileID == "export-dat-lfp-images-v1" {
			ext = "lfp"
		}
		paths[volume.Name+"/LOADFILES/PRODUCTION."+ext] = struct{}{}
	}
	counts := make([]RecipientVolume, len(manifest.Volumes))
	lastVolume := 0
	seenLabels := map[string]bool{}
	for ordinal, doc := range manifest.Documents {
		index, ok := volumeIndex[doc.Volume]
		if !ok || index < lastVolume || index > lastVolume+1 || len(doc.Pages) == 0 ||
			len(doc.Pages) != len(doc.Images) || !safePackagePageLabel(doc.Control, manifest.ProfileID) ||
			!safePackagePageLabel(doc.End, manifest.ProfileID) {
			return nil, ErrRecipientArchive
		}
		lastVolume = index
		stem := fmt.Sprintf("DOC%06d", ordinal+1)
		if doc.TextPath != "TEXT/"+stem+".txt" {
			return nil, ErrRecipientArchive
		}
		paths[doc.Volume+"/"+doc.TextPath] = struct{}{}
		if manifest.ProfileID == "export-dat-pdf-v1" {
			if doc.PDFPath != "PDF/"+stem+".pdf" {
				return nil, ErrRecipientArchive
			}
			paths[doc.Volume+"/"+doc.PDFPath] = struct{}{}
		} else if doc.PDFPath != "" {
			return nil, ErrRecipientArchive
		}
		for pageIndex, page := range doc.Pages {
			image := doc.Images[pageIndex]
			if !safePackagePageLabel(page.Number, manifest.ProfileID) || seenLabels[page.Number] || image.Number != page.Number ||
				image.Path != fmt.Sprintf("IMAGES/%s-%06d.png", stem, pageIndex+1) {
				return nil, ErrRecipientArchive
			}
			seenLabels[page.Number] = true
			paths[doc.Volume+"/"+image.Path] = struct{}{}
		}
		if doc.Control != doc.Pages[0].Number || doc.End != doc.Pages[len(doc.Pages)-1].Number {
			return nil, ErrRecipientArchive
		}
		counts[index].Documents++
		counts[index].Pages += len(doc.Pages)
	}
	for index, volume := range manifest.Volumes {
		if volume.Documents != counts[index].Documents || volume.Pages != counts[index].Pages {
			return nil, ErrRecipientArchive
		}
	}
	return paths, nil
}

func writePackageEntry(archive *zip.Writer, name string, source io.Reader, size int64, expectedSHA string) error {
	header := &zip.FileHeader{Name: name, Method: zip.Store}
	header.SetMode(0o644)
	header.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	destination, err := archive.CreateHeader(header)
	if err != nil {
		return ErrRecipientArchive
	}
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(destination, digest), io.LimitReader(source, size+1))
	if err != nil || n != size {
		return ErrRecipientArchive
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if expectedSHA != "" && actual != expectedSHA {
		return ErrRecipientArchive
	}
	return nil
}

func verifiedExistingPackage(path string, projection PackageProjection) (PackageQC, error) {
	qc, err := VerifyRecipientArchive(path)
	if err != nil || qc.ManifestSHA256 != projection.manifestSHA256 {
		return PackageQC{}, ErrRecipientArchive
	}
	entries := make(map[string]PackageQCEntry, len(qc.Entries))
	for _, entry := range qc.Entries {
		entries[entry.Path] = entry
	}
	for _, binding := range projection.bindings {
		entry, ok := entries[binding.path]
		if !ok || entry.SHA256 != binding.artifact.SHA256 || entry.Size != binding.artifact.Size {
			return PackageQC{}, ErrRecipientArchive
		}
	}
	return qc, nil
}

func packageArchiveEntrySize(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

// BuildRecipientArchive stages one archive, verifies it from disk, then links
// it into place without replacing an existing final archive. QC stays outside.
func BuildRecipientArchive(ctx context.Context, projection PackageProjection, jobID string,
	opener PackageArtifactOpener, destination string) (PackageQC, error) {
	if ctx == nil || opener == nil || jobID == "" || jobID != projection.jobID ||
		destination == "" || projection.manifestSHA256 == "" {
		return PackageQC{}, ErrRecipientArchive
	}
	manifest, err := packageJSON(projection.Manifest)
	if err != nil {
		return PackageQC{}, err
	}
	digest := sha256.Sum256(bytes.TrimSuffix(manifest, []byte{'\n'}))
	if hex.EncodeToString(digest[:]) != projection.manifestSHA256 {
		return PackageQC{}, ErrRecipientArchive
	}
	bindingsDigest, err := packageBindingsDigest(projection.bindings)
	if err != nil || bindingsDigest != projection.bindingsSHA256 {
		return PackageQC{}, ErrRecipientArchive
	}
	expected, err := packageExpectedPaths(projection.Manifest)
	if err != nil || len(projection.bindings) != len(expected)-2-2*len(projection.Manifest.Volumes) {
		return PackageQC{}, ErrRecipientArchive
	}
	bound := map[string]bool{}
	for _, binding := range projection.bindings {
		if _, ok := expected[binding.path]; !ok || bound[binding.path] ||
			strings.Contains(binding.path, "LOADFILES") || binding.artifact.Size < 1 {
			return PackageQC{}, ErrRecipientArchive
		}
		bound[binding.path] = true
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return PackageQC{}, ErrRecipientArchive
		}
		return verifiedExistingPackage(destination, projection)
	} else if !errors.Is(err, os.ErrNotExist) {
		return PackageQC{}, err
	}
	if err := ctx.Err(); err != nil {
		return PackageQC{}, err
	}
	staged, err := os.CreateTemp(filepath.Dir(destination), ".production-archive-*.tmp")
	if err != nil {
		return PackageQC{}, err
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()
	archive := zip.NewWriter(staged)
	write := func(name string, data []byte) error {
		return writePackageEntry(archive, name, bytes.NewReader(data), int64(len(data)), "")
	}
	if err := write("MANIFEST.json", manifest); err != nil {
		return PackageQC{}, err
	}
	transmittal, err := packageJSON(recipientTransmittal(projection.Manifest))
	if err != nil || write("TRANSMITTAL.json", transmittal) != nil {
		return PackageQC{}, ErrRecipientArchive
	}
	for _, volume := range projection.Manifest.Volumes {
		loadfiles, err := makePackageLoadfiles(projection.Manifest, volume)
		if err != nil {
			return PackageQC{}, err
		}
		if err := write(volume.Name+"/LOADFILES/PRODUCTION.dat", loadfiles.dat); err != nil {
			return PackageQC{}, err
		}
		if err := write(volume.Name+"/LOADFILES/PRODUCTION."+loadfiles.ext, loadfiles.page); err != nil {
			return PackageQC{}, err
		}
	}
	for _, binding := range projection.bindings {
		if err := ctx.Err(); err != nil {
			return PackageQC{}, err
		}
		stream, size, err := opener.OpenVerifiedProductionArtifact(ctx, jobID, binding.artifact)
		if err != nil {
			return PackageQC{}, ErrRecipientArchive
		}
		if stream == nil || size != binding.artifact.Size {
			if stream != nil {
				_ = stream.Close()
			}
			return PackageQC{}, ErrRecipientArchive
		}
		err = writePackageEntry(archive, binding.path,
			productionContextReader{ctx: ctx, reader: stream}, size, binding.artifact.SHA256)
		verifyErr := stream.Verify()
		verified := stream.Verified()
		closeErr := stream.Close()
		if err != nil || verifyErr != nil || !verified || closeErr != nil {
			return PackageQC{}, ErrRecipientArchive
		}
	}
	if err := archive.Close(); err != nil {
		return PackageQC{}, ErrRecipientArchive
	}
	if err := staged.Sync(); err != nil {
		return PackageQC{}, err
	}
	if err := staged.Close(); err != nil {
		return PackageQC{}, err
	}
	if err := os.Chmod(staged.Name(), 0o400); err != nil {
		return PackageQC{}, err
	}
	qc, err := VerifyRecipientArchive(staged.Name())
	if err != nil || qc.ManifestSHA256 != projection.manifestSHA256 {
		return PackageQC{}, errors.Join(ErrRecipientArchive, err)
	}
	verifiedEntries := make(map[string]PackageQCEntry, len(qc.Entries))
	for _, entry := range qc.Entries {
		verifiedEntries[entry.Path] = entry
	}
	for _, binding := range projection.bindings {
		entry, ok := verifiedEntries[binding.path]
		if !ok || entry.SHA256 != binding.artifact.SHA256 || entry.Size != binding.artifact.Size {
			return PackageQC{}, ErrRecipientArchive
		}
	}
	if err := os.Link(staged.Name(), destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return verifiedExistingPackage(destination, projection)
		}
		return PackageQC{}, err
	}
	return qc, nil
}

func readPackageEntry(entry *zip.File) ([]byte, error) {
	entrySize, ok := packageArchiveEntrySize(entry.UncompressedSize64)
	if !ok || entrySize > maxPackageMetadataBytes {
		return nil, ErrRecipientArchive
	}
	stream, err := entry.Open()
	if err != nil {
		return nil, ErrRecipientArchive
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, maxPackageMetadataBytes+1))
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil || len(data) > maxPackageMetadataBytes {
		return nil, errors.Join(ErrRecipientArchive, readErr, closeErr)
	}
	return data, nil
}

func digestPackageEntry(entry *zip.File, limit int64) (PackageQCEntry, error) {
	entrySize, ok := packageArchiveEntrySize(entry.UncompressedSize64)
	if !ok || limit < 0 || limit > 50<<30 || entrySize > limit {
		return PackageQCEntry{}, ErrRecipientArchive
	}
	stream, err := entry.Open()
	if err != nil {
		return PackageQCEntry{}, ErrRecipientArchive
	}
	digest := sha256.New()
	n, copyErr := io.Copy(digest, io.LimitReader(stream, limit+1))
	closeErr := stream.Close()
	if copyErr != nil || closeErr != nil || n != entrySize {
		return PackageQCEntry{}, errors.Join(ErrRecipientArchive, copyErr, closeErr)
	}
	return PackageQCEntry{Path: entry.Name, SHA256: hex.EncodeToString(digest.Sum(nil)), Size: n}, nil
}

// VerifyRecipientArchive reopens and checks every entry, the public manifest,
// transmittal, and deterministic per-volume loadfiles independently of the
// private projection and source catalog.
func VerifyRecipientArchive(path string) (PackageQC, error) {
	file, err := os.Open(path)
	if err != nil {
		return PackageQC{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return PackageQC{}, ErrRecipientArchive
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return PackageQC{}, ErrRecipientArchive
	}
	if archive.Comment != "" || len(archive.File) == 0 || len(archive.File) > 1_000_000 {
		return PackageQC{}, ErrRecipientArchive
	}
	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		if _, duplicate := entries[entry.Name]; duplicate || !entry.FileInfo().Mode().IsRegular() ||
			entry.Comment != "" || !bytes.Equal(entry.Extra, packageZIPTimestampExtra) || entry.NonUTF8 ||
			entry.Method != zip.Store || !entry.Modified.Equal(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)) {
			return PackageQC{}, ErrRecipientArchive
		}
		entries[entry.Name] = entry
	}
	manifestEntry := entries["MANIFEST.json"]
	if manifestEntry == nil {
		return PackageQC{}, ErrRecipientArchive
	}
	manifestBytes, err := readPackageEntry(manifestEntry)
	if err != nil {
		return PackageQC{}, err
	}
	var manifest RecipientManifest
	if err := json.Unmarshal(manifestBytes, &manifest, json.RejectUnknownMembers(true)); err != nil {
		return PackageQC{}, ErrRecipientArchive
	}
	canonicalManifest, err := packageJSON(manifest)
	if err != nil || !bytes.Equal(canonicalManifest, manifestBytes) {
		return PackageQC{}, ErrRecipientArchive
	}
	expected, err := packageExpectedPaths(manifest)
	if err != nil || len(entries) != len(expected) {
		return PackageQC{}, ErrRecipientArchive
	}
	for name := range entries {
		if _, ok := expected[name]; !ok {
			return PackageQC{}, ErrRecipientArchive
		}
	}
	transmittal, err := packageJSON(recipientTransmittal(manifest))
	if err != nil {
		return PackageQC{}, err
	}
	actualTransmittal, err := readPackageEntry(entries["TRANSMITTAL.json"])
	if err != nil || !bytes.Equal(actualTransmittal, transmittal) {
		return PackageQC{}, ErrRecipientArchive
	}
	for _, volume := range manifest.Volumes {
		loadfiles, err := makePackageLoadfiles(manifest, volume)
		if err != nil {
			return PackageQC{}, err
		}
		for name, expectedData := range map[string][]byte{
			volume.Name + "/LOADFILES/PRODUCTION.dat":              loadfiles.dat,
			volume.Name + "/LOADFILES/PRODUCTION." + loadfiles.ext: loadfiles.page,
		} {
			actual, err := readPackageEntry(entries[name])
			if err != nil || !bytes.Equal(actual, expectedData) {
				return PackageQC{}, ErrRecipientArchive
			}
		}
	}
	volumeBytes := make(map[string]int64, len(manifest.Volumes))
	for _, doc := range manifest.Documents {
		for _, path := range append([]string{doc.TextPath, doc.PDFPath}, imagePaths(doc.Images)...) {
			if path == "" {
				continue
			}
			entry := entries[doc.Volume+"/"+path]
			if entry == nil {
				return PackageQC{}, ErrRecipientArchive
			}
			entrySize, ok := packageArchiveEntrySize(entry.UncompressedSize64)
			if !ok || entrySize > 50<<30-volumeBytes[doc.Volume] {
				return PackageQC{}, ErrRecipientArchive
			}
			volumeBytes[doc.Volume] += entrySize
		}
	}
	for _, volume := range manifest.Volumes {
		if volumeBytes[volume.Name] != volume.Bytes {
			return PackageQC{}, ErrRecipientArchive
		}
	}
	for _, doc := range manifest.Documents {
		for _, image := range doc.Images {
			if err := verifyPackagePNG(entries[doc.Volume+"/"+image.Path]); err != nil {
				return PackageQC{}, err
			}
		}
	}
	qc := PackageQC{Contract: PackageQCContractV1, Entries: make([]PackageQCEntry, 0, len(entries)),
		PageNumbers: make([]string, 0)}
	manifestDigest := sha256.Sum256(bytes.TrimSuffix(manifestBytes, []byte{'\n'}))
	qc.ManifestSHA256 = hex.EncodeToString(manifestDigest[:])
	for _, doc := range manifest.Documents {
		for _, page := range doc.Pages {
			qc.PageNumbers = append(qc.PageNumbers, page.Number)
		}
	}
	for _, entry := range archive.File {
		limit := int64(maxPackageMetadataBytes)
		if strings.Contains(entry.Name, "/TEXT/") || strings.Contains(entry.Name, "/PDF/") || strings.Contains(entry.Name, "/IMAGES/") {
			volume, _, _ := strings.Cut(entry.Name, "/")
			for _, candidate := range manifest.Volumes {
				if candidate.Name == volume {
					limit = candidate.Bytes
					break
				}
			}
		}
		value, err := digestPackageEntry(entry, limit)
		if err != nil {
			return PackageQC{}, err
		}
		qc.Entries = append(qc.Entries, value)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return PackageQC{}, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, file)
	if err != nil || n != info.Size() {
		return PackageQC{}, ErrRecipientArchive
	}
	qc.ArchiveSHA256 = hex.EncodeToString(digest.Sum(nil))
	return qc, nil
}

func imagePaths(images []RecipientImage) []string {
	paths := make([]string, len(images))
	for index, image := range images {
		paths[index] = image.Path
	}
	return paths
}

func verifyPackagePNG(entry *zip.File) error {
	entrySize, ok := packageArchiveEntrySize(entry.UncompressedSize64)
	if !ok || entrySize > 50<<30 {
		return ErrRecipientArchive
	}
	stream, err := entry.Open()
	if err != nil {
		return ErrRecipientArchive
	}
	config, decodeErr := png.DecodeConfig(stream)
	closeErr := stream.Close()
	if decodeErr != nil || closeErr != nil || config.Width < 1 || config.Height < 1 ||
		int64(config.Width)*int64(config.Height) > 100_000_000 {
		return errors.Join(ErrRecipientArchive, decodeErr, closeErr)
	}
	stream, err = entry.Open()
	if err != nil {
		return ErrRecipientArchive
	}
	_, decodeErr = png.Decode(stream)
	_, drainErr := io.Copy(io.Discard, io.LimitReader(stream, entrySize+1))
	closeErr = stream.Close()
	if decodeErr != nil || drainErr != nil || closeErr != nil {
		return errors.Join(ErrRecipientArchive, decodeErr, drainErr, closeErr)
	}
	return nil
}

// VerifyRecipientArchiveWithQC detects a changed final archive against the
// separately retained receipt rather than trusting its current contents.
func VerifyRecipientArchiveWithQC(path string, expected PackageQC) error {
	actual, err := VerifyRecipientArchive(path)
	if err != nil {
		return err
	}
	if !slices.Equal(actual.Entries, expected.Entries) || !slices.Equal(actual.PageNumbers, expected.PageNumbers) ||
		actual.Contract != expected.Contract || actual.ArchiveSHA256 != expected.ArchiveSHA256 ||
		actual.ManifestSHA256 != expected.ManifestSHA256 {
		return ErrRecipientArchive
	}
	return nil
}
