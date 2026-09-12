package processing

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

type pdfTestRenderer struct{ pdf []byte }

func (r pdfTestRenderer) Render(context.Context, emailpdf.HTML) ([]byte, int64, error) {
	return r.pdf, 1, nil
}

func TestEmailPDFRetainedReceiptReuseAndPhysicalBackup(t *testing.T) {
	for _, released := range []bool{false, true} {
		name := "fresh"
		if released {
			name = "released-v0.9-upgrade"
		}
		t.Run(name, func(t *testing.T) { testEmailPDFRetainedReceiptReuseAndPhysicalBackup(t, released) })
	}
}

func testEmailPDFRetainedReceiptReuseAndPhysicalBackup(t *testing.T, released bool) {
	t.Helper()
	var f emailPipelineFixture
	var target store.EmailTarget
	if released {
		f, target = releasedEmailPDFFixture(t)
	} else {
		f = newEmailPipelineFixture(t)
		target = f.add(t, "synthetic.eml", emailPipelineSource, "message/rfc822")
	}
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	children, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "pdf-child-inventory"))
	require.NoError(t, err)
	require.NotEmpty(t, children.Relations)
	p := fpdf.New("P", "mm", "A4", "")
	p.AddPage()
	p.SetFont("Helvetica", "", 12)
	p.Text(20, 20, "Synthetic integration PDF")
	var out bytes.Buffer
	require.NoError(t, p.Output(&out))
	r := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: pdfTestRenderer{out.Bytes()}, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "151.0.7922.34", RendererSHA256: strings.Repeat("a", 64), FontsSHA256: strings.Repeat("b", 64), Paper: "A4"}}
	r.Recipe.WorkerSHA256 = strings.Repeat("c", 64)
	request, err := r.Request(t.Context(), target.Version.ID, view.Generation.ID, "A4")
	require.NoError(t, err)
	grantWorkerConsent(t, f.catalog, request)
	job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: r, Gate: api.NewOperationGate(), Owner: "email-pdf-test", LeaseDuration: 60_000_000_000, IdleDelay: 1_000_000})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	status, statusErr := f.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, statusErr)
	if status.State != store.RenditionJobCompleted {
		work := store.RenditionJobWork{Job: job, Profile: request.Profile, ExecutionIdentity: request.ExecutionIdentity, Waiter: store.RenditionJobWaiter{ContentVersionID: target.Version.ID}}
		execution, prepErr := r.Prepare(t.Context(), work, time.Now().UTC())
		require.NoError(t, prepErr)
		require.NoError(t, validateRenditionExecution(work, execution))
		result, renderErr := document.RenderRendition(t.Context(), execution.Provider, execution.Upload, execution.Authorization)
		require.NoError(t, renderErr, "status=%+v result=%+v", status, result.Receipt)
		t.Fatalf("worker status=%+v", status)
	}
	receipt, err := f.catalog.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.NoError(t, err)
	retainedReceipts, err := f.catalog.EmailPDFReceipts(t.Context(), target.Version.ID)
	require.NoError(t, err)
	require.Equal(t, []store.EmailPDFReceipt{receipt}, retainedReceipts)
	require.Equal(t, target.Version.BlobHash, receipt.Source.SHA256)
	require.Equal(t, view.Generation.ID, receipt.Binding.GenerationID)
	require.Equal(t, renditionBytesSHA256(out.Bytes()), receipt.Output.PDFSHA256)
	current, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
	require.NoError(t, err)
	require.Equal(t, view.BodySearch, current.BodySearch)
	again, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, job.ID, again.ID)
	require.Equal(t, store.RenditionJobCompleted, again.State)
	_, err = f.catalog.ActiveRendition(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	changed, err := r.Request(t.Context(), target.Version.ID, view.Generation.ID, "Letter")
	require.NoError(t, err)
	require.NotEqual(t, request.Profile.Fingerprint, changed.Profile.Fingerprint)
	replacement, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("Subject: Replacement\r\n\r\nnew-head-marker"))
	require.NoError(t, err)
	_, newVersion, err := f.catalog.ReplaceContent(t.Context(), target.Version.NodeID, store.UnconditionalRev, replacement.Hash, replacement.Size, "message/rfc822", processingBlobPhysical(t, replacement))
	require.NoError(t, err)
	_, err = EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, store.EmailTarget{Version: newVersion})
	require.NoError(t, err)
	historical, err := f.catalog.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, receipt, historical)
	_, _, err = f.catalog.Trash(t.Context(), target.Version.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, _, err = f.catalog.Restore(t.Context(), target.Version.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "email-pdf", f.catalog, f.blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	proof, err := backup.Verify(t.Context(), repository, backupapp.New("email-pdf"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, proof.Problems)
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repository, "email-pdf", backup.RestoreOptions{TargetDir: destination, Jobs: 2})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(destination, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	bs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(destination, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	_, err = maintenance.GarbageCollect(t.Context(), restored, bs, maintenance.GCOptions{})
	require.NoError(t, err)
	got, err := restored.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	stream, _, err := bs.OpenStreamContext(t.Context(), receipt.Output.PDFSHA256)
	require.NoError(t, err)
	retained, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, out.Bytes(), retained)
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	for _, relation := range children.Relations {
		if relation.Child == nil {
			continue
		}
		v, err := restored.ContentVersionByID(t.Context(), relation.Child.VersionID)
		require.NoError(t, err)
		require.Equal(t, relation.Child.SHA256, v.BlobHash)
	}
	// A syntactically valid receipt naming a different selected body must also
	// fail portable metadata validation, not merely the download endpoint.
	var metadata bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &metadata))
	needle := []byte(`"body_sha256":"` + receipt.Output.BodySHA256 + `"`)
	require.Positive(t, bytes.Count(metadata.Bytes(), needle))
	corrupt := bytes.ReplaceAll(metadata.Bytes(), needle, []byte(`"body_sha256":"`+strings.Repeat("f", 64)+`"`))
	imported, err := store.Open(filepath.Join(t.TempDir(), "corrupt-import.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, imported.Close()) }()
	require.Error(t, imported.ImportMetadata(t.Context(), bytes.NewReader(corrupt)))
	_, err = restored.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{target.Version.ID}})
	require.Error(t, err, "retained attachment document authority blocks a source-wide derivative purge")
	_, err = restored.PurgeDerivatives(t.Context(), store.PurgeRequest{AttachmentIDs: []string{receipt.AttachmentID}})
	require.NoError(t, err)
	_, err = restored.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = restored.ContentVersionByID(t.Context(), target.Version.ID)
	require.NoError(t, err, "PDF purge cannot delete the original EML")
	for _, relation := range children.Relations {
		if relation.Child != nil {
			_, err := restored.ContentVersionByID(t.Context(), relation.Child.VersionID)
			require.NoError(t, err, "parent derivative purge cannot delete independent attachment versions")
		}
	}
}

func TestEmailPDFJobRetainsExactReceiptWithoutReplacingBodyHead(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "synthetic.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	before := view.BodySearch
	r := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: pdfTestRenderer{[]byte("malformed PDF")}, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "151.0.7922.34", RendererSHA256: strings.Repeat("a", 64), FontsSHA256: strings.Repeat("b", 64), Paper: "A4"}}
	r.Recipe.WorkerSHA256 = strings.Repeat("c", 64)
	request, err := r.Request(t.Context(), target.Version.ID, view.Generation.ID, "A4")
	require.NoError(t, err)
	grantWorkerConsent(t, f.catalog, request)
	_, _, err = f.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: r, Gate: api.NewOperationGate(), Owner: "email-pdf-test", LeaseDuration: 60_000_000_000, IdleDelay: 1_000_000})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	view, err = f.catalog.EmailMetadata(t.Context(), target.Version.ID)
	require.NoError(t, err)
	require.Equal(t, before, view.BodySearch)
	_, err = f.catalog.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.Error(t, err, "a malformed PDF must never acquire a retained receipt")
}
