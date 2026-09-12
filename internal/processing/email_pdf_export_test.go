package processing

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

// A PDF export must bind the retained receipt, never the current node head or
// a newly selected renderer; a missing receipt cannot become an original role.
func TestExportEmailPDFPinsRetainedReceiptAndArchive(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "synthetic.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: target.Version.NodeID, VersionID: target.Version.ID, SHA256: target.Version.BlobHash, Size: target.Version.Size}},
	}, nil)
	require.NoError(t, err)
	runtime := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Recipe: document.EmailPDFRecipeV1{
		Contract: document.EmailPDFContract, RendererVersion: "synthetic-test", RendererSHA256: strings.Repeat("a", 64), FontsSHA256: strings.Repeat("b", 64), WorkerSHA256: strings.Repeat("c", 64), Paper: "A4",
	}}
	request, err := runtime.Request(t.Context(), target.Version.ID, view.Generation.ID, "A4")
	require.NoError(t, err)
	planRequest := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "email_pdf", ProfileFingerprint: request.Profile.Fingerprint}}}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", planRequest)
	require.ErrorIs(t, err, bundle.ErrUnavailable)

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic retained export PDF")
	var rendered bytes.Buffer
	require.NoError(t, pdf.Output(&rendered))
	runtime.Renderer = pdfTestRenderer{rendered.Bytes()}
	grantWorkerConsent(t, f.catalog, request)
	job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: api.NewOperationGate(), Owner: "synthetic-export", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	status, err := f.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, status.State)
	retained, err := f.catalog.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.NoError(t, err)
	// The failed admission is not silently reinterpreted once a PDF appears.
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", planRequest)
	require.ErrorIs(t, err, bundle.ErrConflict)
	planRequest.OperationID = uuid.NewString()
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", planRequest)
	require.NoError(t, err)
	require.Equal(t, 1, plan.Total)
	require.Equal(t, 1, plan.RoleEntries)
	require.Equal(t, int64(rendered.Len()), plan.RoleBytes)
	walk := func(visit func(bundle.Document) error) error {
		return f.catalog.WalkExportDocuments(t.Context(), plan.ID, visit)
	}
	var documents []bundle.Document
	require.NoError(t, walk(func(d bundle.Document) error { documents = append(documents, d); return nil }))
	require.Len(t, documents, 1)
	require.Len(t, documents[0].Roles, 1)
	role := documents[0].Roles[0]
	require.Equal(t, "email_pdf", role.Role)
	require.Equal(t, "application/pdf", role.MediaType)
	require.Equal(t, retained.Output.PDFSHA256, role.SHA256)
	var binding document.EmailPDFReceiptV1
	require.NoError(t, json.Unmarshal(role.Recipe, &binding, json.RejectUnknownMembers(true)))
	require.Equal(t, retained, binding)
	// Even a newly fingerprinted manifest cannot relabel another message's PDF
	// or change the retained render contract while keeping valid archive bytes.
	for name, mutate := range map[string]func(*document.EmailPDFReceiptV1){
		"source": func(r *document.EmailPDFReceiptV1) { r.Source.VersionID = uuid.NewString() },
		"recipe": func(r *document.EmailPDFReceiptV1) { r.Binding.Recipe.Paper = "Letter" },
		"output": func(r *document.EmailPDFReceiptV1) { r.Output.PDFSHA256 = strings.Repeat("d", 64) },
		"pages":  func(r *document.EmailPDFReceiptV1) { r.Output.Pages = 0 },
	} {
		t.Run("reject-rebound-"+name, func(t *testing.T) {
			changed := documents[0]
			changed.Roles = append([]bundle.Role(nil), changed.Roles...)
			forged := retained
			mutate(&forged)
			changed.Roles[0].Recipe, err = canonical.Marshal(forged)
			require.NoError(t, err)
			changedWalk := func(visit func(bundle.Document) error) error { return visit(changed) }
			changedPlan := plan
			changedPlan.Fingerprint, err = bundle.Fingerprint(changedPlan, changedWalk)
			require.NoError(t, err)
			file, openErr := os.Create(filepath.Join(t.TempDir(), "rebound.zip"))
			require.NoError(t, openErr)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			_, writeErr := bundle.Write(t.Context(), file, changedPlan, changedWalk, func(bundle.Role) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(rendered.Bytes())), nil
			}, nil)
			require.ErrorIs(t, writeErr, bundle.ErrConflict)
		})
	}

	replacement, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("Subject: Changed\r\n\r\nDifferent body"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), target.Version.NodeID, store.UnconditionalRev, replacement.Hash, replacement.Size, "message/rfc822", processingBlobPhysical(t, replacement))
	require.NoError(t, err)
	again, err := f.catalog.CreateExportPlan(t.Context(), "owner", planRequest)
	require.NoError(t, err)
	require.Equal(t, plan, again)
	archive, err := os.Create(filepath.Join(t.TempDir(), "export.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	receipt, err := bundle.Write(t.Context(), archive, plan, walk, func(r bundle.Role) (io.ReadCloser, error) {
		stream, _, openErr := f.blobs.OpenStreamContext(t.Context(), r.SHA256)
		return stream, openErr
	}, nil)
	require.NoError(t, err)
	verified, err := bundle.Verify(t.Context(), archive, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, receipt, verified)
	for _, forged := range []bool{false, true} {
		// Rebuild all ZIP records, CRCs, checksums and the plan fingerprint.
		// The invalid case differs only in its contradictory embedded source.
		changed := documents[0]
		changed.Roles = append([]bundle.Role(nil), changed.Roles...)
		if forged {
			binding.Source.VersionID = uuid.NewString()
			changed.Roles[0].Recipe, err = canonical.Marshal(binding)
			require.NoError(t, err)
		}
		changedPlan := plan
		changedPlan.Fingerprint, err = bundle.Fingerprint(changedPlan, func(visit func(bundle.Document) error) error { return visit(changed) })
		require.NoError(t, err)
		header, err := canonical.Marshal(changedPlan)
		require.NoError(t, err)
		member, err := canonical.Marshal(changed)
		require.NoError(t, err)
		manifest := []byte(fmt.Sprintf("{\"plan\":%s,\"documents\":[%s]}\n", header, member))
		repacked := repackEmailExportManifest(t, archive, receipt.Size, manifest)
		_, err = bundle.Verify(t.Context(), bytes.NewReader(repacked), int64(len(repacked)), changedPlan.Fingerprint)
		if forged {
			require.ErrorIs(t, err, bundle.ErrInvalidArchive)
		} else {
			require.NoError(t, err)
		}
	}
	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	require.NoError(t, restored.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		require.Equal(t, documents[0], d)
		return nil
	}))
}

// Deliberately bypass bundle.Write to exercise the independent reader with an
// attacker-created, checksummed archive. The unchanged-manifest control must pass.
func repackEmailExportManifest(t *testing.T, source io.ReaderAt, size int64, manifest []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(source, size)
	require.NoError(t, err)
	var out, sums bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, entry := range reader.File {
		stream, err := entry.Open()
		require.NoError(t, err)
		payload, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		switch entry.Name {
		case "bundle.json":
			payload = manifest
		case "SHA256SUMS":
			payload = sums.Bytes()
		}
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Exact DOS date required by the portable bundle profile.
		header.SetMode(0600)
		dst, err := writer.CreateHeader(header)
		require.NoError(t, err)
		_, err = dst.Write(payload)
		require.NoError(t, err)
		if entry.Name != "SHA256SUMS" {
			hash := sha256.Sum256(payload)
			_, err = fmt.Fprintf(&sums, "%x  %s\n", hash, entry.Name)
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	return out.Bytes()
}

// A batch shares a renderer recipe, not a source-specific processing profile.
// Changing that recipe after sealing must not select newly rendered bytes.
func TestExportEmailPDFRecipeAcrossDistinctMessages(t *testing.T) {
	f := newEmailPipelineFixture(t)
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic batch PDF")
	var rendered bytes.Buffer
	require.NoError(t, pdf.Output(&rendered))
	recipe := document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "synthetic-batch", RendererSHA256: strings.Repeat("a", 64), FontsSHA256: strings.Repeat("b", 64), WorkerSHA256: strings.Repeat("c", 64), Paper: "A4"}
	runtime := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: pdfTestRenderer{rendered.Bytes()}, Recipe: recipe}
	var members []bundle.Member
	var profiles []string
	for _, name := range []string{"first.eml", "second.eml"} {
		target := f.add(t, name, strings.Replace(emailPipelineSource, "Subject:", "X-Synthetic-Occurrence: "+name+"\r\nSubject:", 1), "message/rfc822")
		view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
		require.NoError(t, err)
		request, err := runtime.Request(t.Context(), target.Version.ID, view.Generation.ID, "A4")
		require.NoError(t, err)
		grantWorkerConsent(t, f.catalog, request)
		job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: api.NewOperationGate(), Owner: "synthetic-batch", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
		require.NoError(t, err)
		_, err = worker.RunOne(t.Context())
		require.NoError(t, err)
		status, err := f.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, status.State)
		members = append(members, bundle.Member{NodeID: target.Version.NodeID, VersionID: target.Version.ID, SHA256: target.Version.BlobHash, Size: target.Version.Size})
		profiles = append(profiles, request.Profile.Fingerprint)
	}
	require.NotEqual(t, profiles[0], profiles[1])
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: members}, nil)
	require.NoError(t, err)
	raw, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	choices, err := f.catalog.ExportEmailPDFRecipes(t.Context(), "owner", source.ID)
	require.NoError(t, err)
	require.Equal(t, source.MemberHash, choices.MemberHash)
	require.Equal(t, []bundle.EmailPDFRecipeChoice{{RecipeSHA256: hex.EncodeToString(digest[:]), Paper: "A4", RendererVersion: recipe.RendererVersion, Messages: 2}}, choices.Recipes)
	request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "email_pdf", RecipeSHA256: hex.EncodeToString(digest[:])}}}
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, 2, plan.Total)
	require.Equal(t, 2, plan.RoleEntries)
	paths := map[string]bool{}
	var count int
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		require.Len(t, d.Roles, 1)
		role := d.Roles[0]
		require.False(t, paths[role.Path])
		paths[role.Path] = true
		var receipt document.EmailPDFReceiptV1
		require.NoError(t, json.Unmarshal(role.Recipe, &receipt))
		require.Equal(t, d.VersionID, receipt.Source.VersionID)
		require.Equal(t, d.SHA256, receipt.Source.SHA256)
		require.Equal(t, recipe, receipt.Binding.Recipe)
		count++
		return nil
	}))
	require.Equal(t, 2, count)
	changed := request
	changed.Roles = []bundle.RolePolicy{{Role: "email_pdf", RecipeSHA256: strings.Repeat("d", 64)}}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", changed)
	require.ErrorIs(t, err, bundle.ErrConflict)
	changed.OperationID = uuid.NewString()
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", changed)
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	changed.OperationID = uuid.NewString()
	changed.Roles[0].AllowUnavailable = true
	partial, err := f.catalog.CreateExportPlan(t.Context(), "owner", changed)
	require.NoError(t, err)
	require.Zero(t, partial.RoleEntries)
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), partial.ID, func(d bundle.Document) error {
		require.Equal(t, "unavailable", d.Roles[0].Status)
		require.Empty(t, d.Roles[0].Path)
		return nil
	}))
}
