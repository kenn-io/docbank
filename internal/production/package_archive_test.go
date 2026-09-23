package production

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/kit/packstore"
)

type syntheticPackageOpener struct{ data map[string][]byte }

type leakingPackageOpener struct{}

func (leakingPackageOpener) OpenVerifiedProductionArtifact(context.Context, string,
	documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	return nil, 0, errors.New("private/source-name and private reason")
}

func syntheticPackagePNG(t *testing.T, page int) []byte {
	t.Helper()
	var body bytes.Buffer
	imageData := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageData.Set(0, 0, color.RGBA{R: uint8(page), G: 42, B: 77, A: 255})
	require.NoError(t, png.Encode(&body, imageData))
	return body.Bytes()
}

func (s syntheticPackageOpener) OpenVerifiedProductionArtifact(_ context.Context, _ string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	data, ok := s.data[artifact.ID]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &syntheticVerifiedPDF{Reader: bytes.NewReader(data), verified: true}, int64(len(data)), nil
}

func packageArchiveFixture(t *testing.T, profile string) (PackageProjection, syntheticPackageOpener) {
	t.Helper()
	job, numbers, members := packageProjectionFixture(t)
	opener := syntheticPackageOpener{data: map[string][]byte{}}
	for i := range job.Manifest.Artifacts {
		artifact := &job.Manifest.Artifacts[i]
		var data []byte
		switch artifact.Role {
		case documentproduction.ArtifactRoleRedactedText:
			data = []byte("synthetic redacted text for " + string(rune('1'+artifact.MemberOrdinal)))
		case documentproduction.ArtifactRoleRedactedPDF:
			data = []byte("%PDF-1.4\nsynthetic redacted PDF\n%%EOF\n")
		case documentproduction.ArtifactRoleRedactedPage:
			data = syntheticPackagePNG(t, artifact.Page)
		}
		digest := sha256.Sum256(data)
		artifact.SHA256, artifact.Size = hex.EncodeToString(digest[:]), int64(len(data))
		opener.data[artifact.ID] = data
	}
	resealPackageJob(t, &job)
	projection, err := PlanPackageProjection(job, numbers, members, profile,
		PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	return projection, opener
}

func requirePackagePrivateReadOnlyMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0o222)
	if runtime.GOOS != "windows" {
		require.Zero(t, info.Mode().Perm()&0o077)
	}
}

func TestPackageArchiveEntrySizeRejectsOverflow(t *testing.T) {
	size, ok := packageArchiveEntrySize(uint64(math.MaxInt64))
	require.True(t, ok)
	require.Equal(t, int64(math.MaxInt64), size)
	_, ok = packageArchiveEntrySize(uint64(math.MaxInt64) + 1)
	require.False(t, ok)
}

func TestBuildRecipientArchiveReopensAndVerifiesAllProfiles(t *testing.T) {
	for _, profile := range []string{"export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1"} {
		t.Run(profile, func(t *testing.T) {
			projection, opener := packageArchiveFixture(t, profile)
			first := filepath.Join(t.TempDir(), "first.zip")
			qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, first)
			require.NoError(t, err)
			requirePackagePrivateReadOnlyMode(t, first)
			require.Equal(t, projection.PageNumbers(), qc.PageNumbers)
			verified, err := VerifyRecipientArchive(first)
			require.NoError(t, err)
			require.Equal(t, qc, verified)
			second := filepath.Join(t.TempDir(), "second.zip")
			_, err = BuildRecipientArchive(t.Context(), projection, packageJobID, opener, second)
			require.NoError(t, err)
			a, err := os.ReadFile(first)
			require.NoError(t, err)
			b, err := os.ReadFile(second)
			require.NoError(t, err)
			require.Equal(t, a, b)
			for _, secret := range []string{packageOneID, packageTwoID, packageJobID, "private/", "source_sha256", "reason"} {
				require.NotContains(t, string(a), secret)
			}
			reader, err := zip.OpenReader(first)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			for _, entry := range reader.File {
				require.NotContains(t, entry.Name, "private")
				stream, openErr := entry.Open()
				require.NoError(t, openErr)
				body, readErr := io.ReadAll(stream)
				require.NoError(t, readErr)
				require.NoError(t, stream.Close())
				for _, secret := range []string{packageOneID, packageTwoID, packageJobID, "private/", "source_sha256", "reason"} {
					require.NotContains(t, string(body), secret)
				}
			}
		})
	}
}

func TestRecipientLoadfilePreflightKeepsDocumentIdentityAndLabel(t *testing.T) {
	projection, _ := packageArchiveFixture(t, "export-dat-pdf-v1")
	loadfiles, err := makePackageLoadfiles(projection.Manifest, projection.Manifest.Volumes[0])
	require.NoError(t, err)
	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	records, diagnostics, err := loadfile.ParseDAT(bytes.NewReader(loadfiles.dat), profile)
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	mapping, _, err := loadfile.DecodeMapping([]byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","canonical":"loadfile.document.id"},{"source":"BEGDOC","canonical":"loadfile.label.begin"},{"source":"ENDDOC","canonical":"loadfile.label.end"},{"source":"TEXT","canonical":"loadfile.file.supplied_text"},{"source":"PDF","canonical":"loadfile.file.produced_pdf"}]}`), records[0].ColumnOrder)
	require.NoError(t, err)
	_, err = loadfile.ApplyMapping(records, mapping,
		profile, func(int64) error { return nil })
	require.NoError(t, err)
	require.Len(t, records, 2)
	for index, record := range records {
		require.Equal(t, projection.Manifest.Documents[index].Control, record.DocID)
		require.Equal(t, projection.Manifest.Documents[index].Control, record.Fields[1].Raw)
		require.Equal(t, "loadfile.label.begin", record.Fields[1].Canonical)
	}
}

func TestRecipientArchivePublishesImportMapping(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-pdf-v1")
	path := filepath.Join(t.TempDir(), "production.zip")
	_, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
	require.NoError(t, err)
	archive, err := zip.OpenReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	var mappingJSON, dat []byte
	for _, entry := range archive.File {
		if entry.Name != "MAPPING.json" && entry.Name != "VOL001/LOADFILES/PRODUCTION.dat" {
			continue
		}
		stream, openErr := entry.Open()
		require.NoError(t, openErr)
		body, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		require.NoError(t, stream.Close())
		if entry.Name == "MAPPING.json" {
			mappingJSON = body
		} else {
			dat = body
		}
	}
	require.NotEmpty(t, mappingJSON)
	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	records, diagnostics, err := loadfile.ParseDAT(bytes.NewReader(dat), profile)
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	mapping, _, err := loadfile.DecodeMapping(mappingJSON, records[0].ColumnOrder)
	require.NoError(t, err)
	_, err = loadfile.ApplyMapping(records, mapping, profile, func(int64) error { return nil })
	require.NoError(t, err)
	require.Equal(t, projection.Manifest.Documents[0].Control, records[0].DocID)
	require.Equal(t, "loadfile.label.begin", records[0].Fields[1].Canonical)
	require.Equal(t, "loadfile.label.end", records[0].Fields[2].Canonical)
	require.Equal(t, []string{"produced_pdf", "supplied_text"},
		[]string{records[0].Files[0].Role, records[0].Files[1].Role})
}

func TestRecipientManifestDeclaresAndVerifiesOutputDigests(t *testing.T) {
	for _, profile := range []string{"export-dat-pdf-v1", "export-dat-opt-images-v1"} {
		t.Run(profile, func(t *testing.T) {
			projection, opener := packageArchiveFixture(t, profile)
			path := filepath.Join(t.TempDir(), "production.zip")
			qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
			require.NoError(t, err)
			entries := make(map[string]PackageQCEntry, len(qc.Entries))
			for _, entry := range qc.Entries {
				entries[entry.Path] = entry
			}
			for _, doc := range projection.Manifest.Documents {
				text := entries[doc.Volume+"/"+doc.TextPath]
				require.Equal(t, text.SHA256, doc.TextSHA256)
				require.Equal(t, text.Size, doc.TextSize)
				if doc.PDFPath != "" {
					pdf := entries[doc.Volume+"/"+doc.PDFPath]
					require.Equal(t, pdf.SHA256, doc.PDFSHA256)
					require.Equal(t, pdf.Size, doc.PDFSize)
				}
				for _, image := range doc.Images {
					entry := entries[doc.Volume+"/"+image.Path]
					require.Equal(t, entry.SHA256, image.SHA256)
					require.Equal(t, entry.Size, image.Size)
				}
			}
			original, err := zip.OpenReader(path)
			require.NoError(t, err)
			changedPath := filepath.Join(t.TempDir(), "changed.zip")
			changed, err := os.Create(changedPath)
			require.NoError(t, err)
			writer := zip.NewWriter(changed)
			for _, entry := range original.File {
				header := entry.FileHeader
				destination, createErr := writer.CreateHeader(&header)
				require.NoError(t, createErr)
				source, openErr := entry.Open()
				require.NoError(t, openErr)
				body, readErr := io.ReadAll(source)
				require.NoError(t, readErr)
				require.NoError(t, source.Close())
				if entry.Name == projection.Manifest.Documents[0].Volume+"/"+projection.Manifest.Documents[0].TextPath {
					body = bytes.Repeat([]byte{'X'}, len(body))
				}
				_, writeErr := destination.Write(body)
				require.NoError(t, writeErr)
			}
			require.NoError(t, writer.Close())
			require.NoError(t, changed.Close())
			require.NoError(t, original.Close())
			_, err = VerifyRecipientArchive(changedPath)
			require.ErrorIs(t, err, ErrRecipientArchive)
		})
	}
}

func TestBuildRecipientArchiveRejectsMissingChangedAndReturnsPublishedDestination(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	destination := filepath.Join(t.TempDir(), "production.zip")
	missing := opener.data[projection.bindings[0].artifact.ID]
	delete(opener.data, projection.bindings[0].artifact.ID)
	_, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, destination)
	require.Error(t, err)
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
	opener.data[projection.bindings[0].artifact.ID] = []byte("changed")
	_, err = BuildRecipientArchive(t.Context(), projection, packageJobID, opener, destination)
	require.Error(t, err)
	opener.data[projection.bindings[0].artifact.ID] = missing
	firstQC, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, destination)
	require.NoError(t, err)
	firstInfo, err := os.Stat(destination)
	require.NoError(t, err)
	// Simulate a lost response followed by a retry after staged source bytes
	// are unavailable. The already published archive remains the authority.
	retriedQC, err := BuildRecipientArchive(t.Context(), projection, packageJobID,
		syntheticPackageOpener{}, destination)
	require.NoError(t, err)
	require.Equal(t, firstQC, retriedQC)
	secondInfo, err := os.Stat(destination)
	require.NoError(t, err)
	require.True(t, os.SameFile(firstInfo, secondInfo))
}

func TestRecipientArchiveRejectsUnapprovedZIPMetadata(t *testing.T) {
	for _, change := range []string{"archive comment", "entry comment", "entry extra"} {
		t.Run(change, func(t *testing.T) {
			projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
			path := filepath.Join(t.TempDir(), "production.zip")
			_, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
			require.NoError(t, err)
			original, err := zip.OpenReader(path)
			require.NoError(t, err)
			changedPath := filepath.Join(t.TempDir(), "changed.zip")
			changed, err := os.Create(changedPath)
			require.NoError(t, err)
			writer := zip.NewWriter(changed)
			if change == "archive comment" {
				require.NoError(t, writer.SetComment("private source name"))
			}
			for index, entry := range original.File {
				header := entry.FileHeader
				if index == 0 && change == "entry comment" {
					header.Comment = "private source name"
				}
				if index == 0 && change == "entry extra" {
					header.Extra = []byte{0xfe, 0xca, 4, 0, 'p', 'r', 'i', 'v'}
				}
				destination, createErr := writer.CreateHeader(&header)
				require.NoError(t, createErr)
				source, openErr := entry.Open()
				require.NoError(t, openErr)
				_, copyErr := io.CopyN(destination, source, int64(entry.UncompressedSize64))
				require.NoError(t, copyErr)
				require.NoError(t, source.Close())
			}
			require.NoError(t, writer.Close())
			require.NoError(t, changed.Close())
			require.NoError(t, original.Close())
			_, err = VerifyRecipientArchive(changedPath)
			require.ErrorIs(t, err, ErrRecipientArchive)
			_, err = BuildRecipientArchive(t.Context(), projection, packageJobID, syntheticPackageOpener{}, changedPath)
			require.ErrorIs(t, err, ErrRecipientArchive)
		})
	}
}

func TestBuildRecipientArchiveRejectsDifferentJobIdentity(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	path := filepath.Join(t.TempDir(), "production.zip")
	_, err := BuildRecipientArchive(t.Context(), projection, "different-job", opener, path)
	require.ErrorIs(t, err, ErrRecipientArchive)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
	require.NoError(t, err)
	_, err = BuildRecipientArchive(t.Context(), projection, "different-job", syntheticPackageOpener{}, path)
	require.ErrorIs(t, err, ErrRecipientArchive)
}

func TestBuildRecipientArchiveRejectsChangedProjectionAndRoleBinding(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*PackageProjection)
	}{
		{"public label", func(p *PackageProjection) { p.Manifest.Documents[0].Pages[0].Number = "changed" }},
		{"private role", func(p *PackageProjection) { p.bindings[0].artifact.Role = "original" }},
		{"private source", func(p *PackageProjection) { p.bindings[0].artifact.Path = "private/changed" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
			change.edit(&projection)
			path := filepath.Join(t.TempDir(), "production.zip")
			_, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
			require.Error(t, err)
			_, err = os.Stat(path)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestBuildRecipientArchiveDoesNotExposeSourceOpenErrors(t *testing.T) {
	projection, _ := packageArchiveFixture(t, "export-dat-opt-images-v1")
	_, err := BuildRecipientArchive(t.Context(), projection, packageJobID, leakingPackageOpener{},
		filepath.Join(t.TempDir(), "production.zip"))
	require.ErrorIs(t, err, ErrRecipientArchive)
	require.NotContains(t, err.Error(), "private")
}

func TestBuildRecipientArchiveKeepsVolumeAndLoadfileOrder(t *testing.T) {
	for _, profileID := range []string{"export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1"} {
		t.Run(profileID, func(t *testing.T) {
			job, numbers, members := packageProjectionFixture(t)
			members[1].FamilyID = "family-two"
			opener := syntheticPackageOpener{data: map[string][]byte{}}
			for index := range job.Manifest.Artifacts {
				artifact := &job.Manifest.Artifacts[index]
				body := []byte("synthetic output " + artifact.Role + " " + string(rune('0'+artifact.Page)))
				if artifact.Role == documentproduction.ArtifactRoleRedactedPage {
					body = syntheticPackagePNG(t, artifact.Page)
				}
				digest := sha256.Sum256(body)
				artifact.SHA256, artifact.Size = hex.EncodeToString(digest[:]), int64(len(body))
				opener.data[artifact.ID] = body
			}
			resealPackageJob(t, &job)
			projection, err := PlanPackageProjection(job, numbers, members, profileID,
				PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 1})
			require.NoError(t, err)
			require.Len(t, projection.Manifest.Volumes, 2)
			path := filepath.Join(t.TempDir(), "volumes.zip")
			qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
			require.NoError(t, err)
			require.NoError(t, VerifyRecipientArchiveWithQC(path, qc))
			archive, err := zip.OpenReader(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			for _, volume := range projection.Manifest.Volumes {
				var datBytes, pageBytes []byte
				for _, entry := range archive.File {
					if entry.Name == volume.Name+"/LOADFILES/PRODUCTION.dat" ||
						strings.HasPrefix(entry.Name, volume.Name+"/LOADFILES/PRODUCTION.") {
						stream, openErr := entry.Open()
						require.NoError(t, openErr)
						body, readErr := io.ReadAll(stream)
						require.NoError(t, readErr)
						require.NoError(t, stream.Close())
						if strings.HasSuffix(entry.Name, ".dat") {
							datBytes = body
						} else {
							pageBytes = body
						}
					}
				}
				datProfile, err := loadfile.ReadProfile("dat-concordance-v1")
				require.NoError(t, err)
				datProfile.Columns = []string{"DOCID", "BEGDOC", "ENDDOC", "VOLUME", "PAGES", "PDF", "TEXT", "FIRST_IMAGE"}
				var controls []string
				datDiagnostics, err := loadfile.ScanDAT(bytes.NewReader(datBytes), datProfile, func(record loadfile.Record) error {
					controls = append(controls, record.Fields[0].Raw)
					return nil
				})
				require.NoError(t, err)
				require.Empty(t, datDiagnostics)
				var images []loadfile.ImageRef
				if profileID == "export-dat-lfp-images-v1" {
					pageProfile, profileErr := loadfile.ReadProfile("lfp-ipro-v1")
					require.NoError(t, profileErr)
					require.NoError(t, loadfile.ScanLFP(t.Context(), bytes.NewReader(pageBytes), pageProfile,
						func(image loadfile.ImageRef) error { images = append(images, image); return nil }))
				} else {
					pageProfile, profileErr := loadfile.ReadProfile("opt-standard-v1")
					require.NoError(t, profileErr)
					optDiagnostics, scanErr := loadfile.ScanOPT(t.Context(), bytes.NewReader(pageBytes), pageProfile,
						func(image loadfile.ImageRef) error { images = append(images, image); return nil })
					require.NoError(t, scanErr)
					require.Empty(t, optDiagnostics)
				}
				var expectedControls, expectedPages []string
				for _, doc := range projection.Manifest.Documents {
					if doc.Volume != volume.Name {
						continue
					}
					expectedControls = append(expectedControls, doc.Control)
					for _, page := range doc.Pages {
						expectedPages = append(expectedPages, page.Number)
					}
				}
				require.Equal(t, expectedControls, controls)
				require.Len(t, images, len(expectedPages))
				for index, image := range images {
					require.Equal(t, expectedPages[index], image.ImageKey)
					require.Equal(t, volume.Name, image.Volume)
					require.Contains(t, image.RelPath, "IMAGES/")
				}
			}
		})
	}
}

func TestRecipientArchiveQCDetectsChangedFinalBytes(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	path := filepath.Join(t.TempDir(), "production.zip")
	qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, path)
	require.NoError(t, err)
	archive, err := os.ReadFile(path)
	require.NoError(t, err)
	archive[len(archive)-1] ^= 1
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, archive, 0o600))
	require.Error(t, VerifyRecipientArchiveWithQC(path, qc))
}

func TestBuildRecipientArchiveRejectsNonPNGPageRoleBytes(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	for index := range projection.bindings {
		binding := &projection.bindings[index]
		if binding.artifact.Role != documentproduction.ArtifactRoleRedactedPage {
			continue
		}
		body := []byte("not a PNG")
		digest := sha256.Sum256(body)
		projection.Manifest.Volumes[0].Bytes += int64(len(body)) - binding.artifact.Size
		binding.artifact.SHA256, binding.artifact.Size = hex.EncodeToString(digest[:]), int64(len(body))
		opener.data[binding.artifact.ID] = body
		break
	}
	var err error
	projection.bindingsSHA256, err = packageBindingsDigest(projection.bindings)
	require.NoError(t, err)
	manifest, err := packageJSON(projection.Manifest)
	require.NoError(t, err)
	manifestDigest := sha256.Sum256(bytes.TrimSuffix(manifest, []byte{'\n'}))
	projection.manifestSHA256 = hex.EncodeToString(manifestDigest[:])
	_, err = BuildRecipientArchive(t.Context(), projection, packageJobID, opener, filepath.Join(t.TempDir(), "bad.zip"))
	require.Error(t, err)
}
