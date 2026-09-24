package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

type recipientImportOpener struct{ data map[string][]byte }

type recipientImportStream struct{ *bytes.Reader }

func (s *recipientImportStream) Close() error { return nil }
func (s *recipientImportStream) Verify() error {
	if s.Len() != 0 {
		return errors.New("synthetic stream was not fully read")
	}
	return nil
}
func (s *recipientImportStream) Verified() bool { return s.Len() == 0 }

func (o recipientImportOpener) OpenVerifiedProductionArtifact(_ context.Context, _ string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	data, ok := o.data[artifact.ID]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &recipientImportStream{Reader: bytes.NewReader(data)}, int64(len(data)), nil
}

func recipientImportHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func syntheticRecipientArchive(t *testing.T, memberCount, maxVolumeDocuments int) (string, production.RecipientManifest) {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "Retained Synthetic Phrase")
	var pdfBody bytes.Buffer
	require.NoError(t, pdf.Output(&pdfBody))
	var imageBody bytes.Buffer
	page := image.NewRGBA(image.Rect(0, 0, 2, 2))
	page.Set(0, 0, color.RGBA{R: 20, G: 40, B: 60, A: 255})
	require.NoError(t, png.Encode(&imageBody, page))
	textBody := []byte("Retained Synthetic Phrase\f")

	jobID, setID := uuid.NewString(), uuid.NewString()
	privateCanary := "OMITTED-SYNTHETIC-CANARY"
	revisionSHA := recipientImportHash([]byte("synthetic revision"))
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
		Authority: "synthetic-ledger", ID: uuid.NewString(), OperationID: jobID, RevisionSHA256: revisionSHA,
		State: "reserved"}
	opener := recipientImportOpener{data: map[string][]byte{}}
	artifacts := []documentproduction.Artifact{}
	members := []production.PackageMember{}
	for index := range memberCount {
		memberID := uuid.NewString()
		ordinal := int64(index + 1)
		members = append(members, production.PackageMember{ID: memberID, Ordinal: ordinal,
			FamilyID: "synthetic-family-" + memberID})
		reservation.Numbers = append(reservation.Numbers, documentproduction.AssignedNumber{
			MemberID: memberID, MemberOrdinal: ordinal, Page: 1, Text: fmt.Sprintf("OUT%06d", 41+index)})
		for _, part := range []struct {
			role, media string
			page        int
			data        []byte
		}{
			{documentproduction.ArtifactRoleRedactedPDF, "application/pdf", 0, pdfBody.Bytes()},
			{documentproduction.ArtifactRoleRedactedText, "text/plain; charset=utf-8", 0, textBody},
			{documentproduction.ArtifactRoleRedactedPage, "image/png", 1, imageBody.Bytes()},
		} {
			artifact := documentproduction.Artifact{ID: uuid.NewString(), MemberID: memberID, MemberOrdinal: ordinal,
				Role: part.role, Page: part.page, Path: "private/" + privateCanary + "-" + memberID + "-" + part.role,
				SHA256: recipientImportHash(part.data), Size: int64(len(part.data)), MediaType: part.media}
			artifacts = append(artifacts, artifact)
			opener.data[artifact.ID] = part.data
		}
	}
	var err error
	_, reservation.SHA256, err = documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1,
		Artifacts: artifacts}
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	require.NoError(t, err)
	preparedSHA := recipientImportHash([]byte("synthetic prepared input"))
	receipt := documentproduction.ProductionReceipt{Contract: documentproduction.ProductionReceiptContractV1,
		ID: jobID, JobID: jobID, SetID: setID, Revision: 1, RevisionSHA256: revisionSHA,
		PreparedInputSHA256: preparedSHA, PolicySHA256: recipientImportHash([]byte("synthetic policy")),
		NumberReservationSHA256: reservation.SHA256, LayoutSHA256: recipientImportHash([]byte("synthetic layout")),
		EndorsementsSHA256:     recipientImportHash([]byte("synthetic endorsement")),
		ArtifactManifestSHA256: manifest.SHA256, CreatedAt: "2026-09-23T12:00:00Z"}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	job := production.Job{ID: jobID, SetID: setID, Revision: 1, RevisionSHA256: revisionSHA,
		PreparedInputSHA256: preparedSHA, State: production.ProductionJobSucceeded,
		Receipt: receipt, Manifest: manifest}
	projection, err := production.PlanPackageProjection(job, reservation, members,
		"export-dat-pdf-v1", production.PackageLimits{MaxVolumeBytes: 10 << 20, MaxVolumeDocuments: maxVolumeDocuments})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "recipient.zip")
	_, err = production.BuildRecipientArchive(t.Context(), projection, jobID, opener, path)
	require.NoError(t, err)
	return path, projection.Manifest
}

func TestRecipientProductionPackageImportsIntoFreshVault(t *testing.T) {
	for _, fixture := range []struct {
		name                      string
		count, maxVolumeDocuments int
	}{
		{name: "one volume", count: 1, maxVolumeDocuments: 10},
		{name: "two volumes", count: 2, maxVolumeDocuments: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			memberCount := fixture.count
			path, manifest := syntheticRecipientArchive(t, memberCount, fixture.maxVolumeDocuments)
			require.Len(t, manifest.Volumes, memberCount)
			_, err := production.VerifyRecipientArchive(path)
			require.NoError(t, err)
			archive, err := zip.OpenReader(path)
			require.NoError(t, err)
			var pdfBody []byte
			for _, entry := range archive.File {
				require.NotContains(t, entry.Name, "OMITTED-SYNTHETIC-CANARY")
				stream, openErr := entry.Open()
				require.NoError(t, openErr)
				body, readErr := io.ReadAll(stream)
				require.NoError(t, readErr)
				require.NotContains(t, string(body), "OMITTED-SYNTHETIC-CANARY")
				if entry.Name == manifest.Documents[0].Volume+"/"+manifest.Documents[0].PDFPath {
					pdfBody = body
				}
				require.NoError(t, stream.Close())
			}
			require.NoError(t, archive.Close())
			visible, err := packagetest.PDFText(t.Context(), pdfBody)
			require.NoError(t, err)
			require.Contains(t, visible, "Retained Synthetic Phrase")
			require.NotContains(t, visible, "OMITTED-SYNTHETIC-CANARY")

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			root, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
			mapping, err := os.ReadFile(filepath.Join(root, "MAPPING.json"))
			require.NoError(t, err)
			srv, catalog := newPackageTestServer(t)
			preview := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{Profile: "dat-concordance-v1",
				PageMapProfile: "opt-standard-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
				Mapping: mapping}))
			require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
			var preflight api.PackagePreflight
			require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &preflight))
			require.False(t, preflight.Blocking, "%+v", preflight.Diagnostics)
			require.Equal(t, memberCount, preflight.Records)
			require.Equal(t, memberCount, preflight.Pages)
			operationID := uuid.NewString()
			admitted := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t,
				api.PackageImportRequest{PreflightID: preflight.PreflightID, Into: "/", Name: "synthetic-recipient",
					OperationID: operationID, IndexSuppliedText: true}), nil)
			require.Equal(t, http.StatusAccepted, admitted.Code, admitted.Body.String())
			worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
				Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker",
				LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
				Mutate: func(_ context.Context, fn func() error) error { return fn() },
			})
			require.NoError(t, err)
			_, err = worker.ProcessOnce(t.Context())
			require.NoError(t, err)
			var job api.PackageImportJob
			require.NoError(t, json.Unmarshal(admitted.Body.Bytes(), &job))
			pkg, err := catalog.Package(t.Context(), job.PackageID)
			require.NoError(t, err)
			require.Equal(t, "complete", pkg.State)
			manifestStream, _, err := catalog.Blobs.OpenStreamContext(t.Context(), pkg.ManifestBlobSHA256)
			require.NoError(t, err)
			frozen, err := loadfile.ReadManifestJSONL(manifestStream, pkg.ManifestSHA256)
			require.NoError(t, err)
			require.NoError(t, manifestStream.Close())
			var rawLoadfiles int
			for _, file := range frozen.Files {
				if file.Role == "raw_load_file" {
					rawLoadfiles++
				}
			}
			require.Equal(t, 2*memberCount, rawLoadfiles)
			members, err := catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
			require.NoError(t, err)
			require.Len(t, members, memberCount)
			for index, member := range members {
				require.Equal(t, manifest.Documents[index].PDFSHA256, member.BlobSHA256)
				var indexedText, producedPDF bool
				for _, representation := range member.Representations {
					if representation.Role == "supplied_text" && representation.Status == "available" &&
						representation.LexicalGenerationID != "" {
						indexedText = true
					}
					if representation.Role == "produced_pdf" && representation.Status == "available" &&
						representation.BlobSHA256 == manifest.Documents[index].PDFSHA256 {
						producedPDF = true
					}
				}
				require.True(t, indexedText)
				require.True(t, producedPDF)
			}
			for _, query := range []struct {
				term string
				want int
			}{{"Retained", memberCount}, {"OMITTED-SYNTHETIC-CANARY", 0}} {
				response := srv.get(t, "/api/v1/search?q="+query.term+"&limit=10")
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var report api.SearchReport
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
				require.Len(t, report.Hits, query.want)
			}
		})
	}
}

func TestRecipientPreflightRejectsMismatchedVolumeColumns(t *testing.T) {
	path, _ := syntheticRecipientArchive(t, 2, 1)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	root, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
	datPath := filepath.Join(root, "VOL002", "LOADFILES", "PRODUCTION.dat")
	dat, err := os.ReadFile(datPath)
	require.NoError(t, err)
	changed := bytes.Replace(dat, []byte("DOCID"), []byte("DOCNO"), 1)
	require.NotEqual(t, dat, changed)
	require.NoError(t, os.WriteFile(datPath, changed, 0o600))
	mapping, err := os.ReadFile(filepath.Join(root, "MAPPING.json"))
	require.NoError(t, err)
	srv, _ := newPackageTestServer(t)
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{Profile: "dat-concordance-v1",
		PageMapProfile: "opt-standard-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
		Mapping: mapping}))
	require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "different column order")
}

func TestRecipientImportRejectsSecondVolumeChangedAfterPreflight(t *testing.T) {
	path, _ := syntheticRecipientArchive(t, 2, 1)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	root, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
	mapping, err := os.ReadFile(filepath.Join(root, "MAPPING.json"))
	require.NoError(t, err)
	srv, catalog := newPackageTestServer(t)
	preview := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{Profile: "dat-concordance-v1",
		PageMapProfile: "opt-standard-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
		Mapping: mapping}))
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	var preflight api.PackagePreflight
	require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &preflight))
	require.False(t, preflight.Blocking)
	admitted := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t,
		api.PackageImportRequest{PreflightID: preflight.PreflightID, Into: "/", Name: "synthetic-recipient",
			OperationID: uuid.NewString(), IndexSuppliedText: true}), nil)
	require.Equal(t, http.StatusAccepted, admitted.Code, admitted.Body.String())
	datPath := filepath.Join(root, "VOL002", "LOADFILES", "PRODUCTION.dat")
	dat, err := os.ReadFile(datPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(datPath, append(dat, '\n'), 0o600))
	worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
		Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Mutate: func(_ context.Context, fn func() error) error { return fn() },
	})
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.ErrorContains(t, err, "changed after preflight")
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal(admitted.Body.Bytes(), &job))
	pkg, err := catalog.Package(t.Context(), job.PackageID)
	require.NoError(t, err)
	require.NotEqual(t, "complete", pkg.State)
}
