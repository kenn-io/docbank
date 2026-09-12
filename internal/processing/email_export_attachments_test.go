package processing

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
)

// Missing attachment expansion must fail this test: each published occurrence
// gets its own bounded receipt row and independently checked archive bytes.
func TestExportOriginalAttachmentOccurrences(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "source.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	publication, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "export-attachments"))
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "attachment_original"}}}
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, 1, plan.Total)
	require.Equal(t, 3, plan.RoleEntries)
	var rows []bundle.Document
	walk := func(visit func(bundle.Document) error) error {
		return f.catalog.WalkExportDocuments(t.Context(), plan.ID, visit)
	}
	require.NoError(t, walk(func(d bundle.Document) error { rows = append(rows, d); return nil }))
	require.Len(t, rows, 3)
	for i, rel := range publication.Relations {
		require.Len(t, rows[i+1].Roles, 1)
		require.Equal(t, rel.Child.SHA256, rows[i+1].Roles[0].SHA256)
		raw, e := json.Marshal(rows[i+1])
		require.NoError(t, e)
		require.Less(t, len(raw), bundle.MaxMemberBytes)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "attachments.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	receipt, err := bundle.Write(t.Context(), file, plan, walk, func(r bundle.Role) (io.ReadCloser, error) {
		stream, _, e := f.blobs.OpenStreamContext(t.Context(), r.SHA256)
		return stream, e
	}, nil)
	require.NoError(t, err)
	_, err = bundle.Verify(t.Context(), file, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	preview, err := f.catalog.ExportPlanPreview(t.Context(), "owner", plan.ID)
	require.NoError(t, err)
	require.Equal(t, 1, preview.Total)
	require.Equal(t, 2, preview.Roles[1].Files)
	// Releasing publication authority while its exact relations are sealed would
	// make a later backup or retry lose the source of the child bindings.
	err = f.catalog.RemoveEmailDocumentPublication(t.Context(), publication.OperationID, publication.RequestDigest)
	require.ErrorIs(t, err, bundle.ErrRetained)
	for name, change := range map[string]func([]bundle.Document) []bundle.Document{
		"missing occurrence":  func(ds []bundle.Document) []bundle.Document { return ds[:2] },
		"repeated occurrence": func(ds []bundle.Document) []bundle.Document { ds[2] = ds[1]; return ds },
		"foreign child": func(ds []bundle.Document) []bundle.Document {
			rel := *ds[1].Attachment
			child := *rel.Child
			child.VersionID = uuid.NewString()
			rel.Child = &child
			ds[1].Attachment = &rel
			ds[1].Roles[0].SHA256 = strings.Repeat("f", 64)
			return ds
		},
		"foreign publication": func(ds []bundle.Document) []bundle.Document {
			rel := *ds[1].Attachment
			rel.OperationID = "other"
			ds[1].Attachment = &rel
			return ds
		},
	} {
		t.Run(name, func(t *testing.T) {
			ds := slices.Clone(rows)
			for i := range ds {
				ds[i].Roles = slices.Clone(ds[i].Roles)
			}
			ds = change(ds)
			validator := bundle.RowValidator{Plan: plan}
			var failure error
			for _, d := range ds {
				if failure = validator.Add(d); failure != nil {
					break
				}
			}
			if failure == nil {
				failure = validator.Finish()
			}
			require.ErrorIs(t, failure, bundle.ErrConflict)
		})
	}
}

func TestExportAttachmentVolumeJobRetryAndReadback(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "volumes.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	_, err = PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "volume-attachments"))
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	r := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "attachment_original"}}, VolumeLimits: &bundle.VolumeLimits{Roles: 1, RoleBytes: 1 << 20}}
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", r)
	require.NoError(t, err)
	require.Equal(t, 3, plan.Volumes)
	again, err := f.catalog.CreateExportPlan(t.Context(), "owner", r)
	require.NoError(t, err)
	require.Equal(t, plan, again)
	request := bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}
	job, err := f.catalog.QueueExportJob(t.Context(), "owner", request)
	require.NoError(t, err)
	worker, err := exporter.New(f.catalog, f.blobs, t.TempDir(), api.NewOperationGate())
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	completed, err := f.catalog.QueueExportJob(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, job.ID, completed.ID)
	require.Equal(t, "completed", completed.State)
	require.Equal(t, 3, completed.CompletedRoles)
	require.Equal(t, plan.RoleBytes, completed.CompletedBytes)
	require.Equal(t, 6, completed.Receipt.Entries)
	file, receipt, release, err := worker.Lease(t.Context(), "owner", job.ID)
	require.NoError(t, err)
	defer release()
	_, err = bundle.Verify(t.Context(), file, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "volume-restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
}

func TestExportQualifiedAttachmentPDFAndUnsupportedChild(t *testing.T) {
	f := newEmailPipelineFixture(t)
	raw := "Subject: Parent\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nParent body\r\n--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=child.eml\r\n\r\nSubject: Nested child\r\nContent-Type: text/plain\r\n\r\nNested child body\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=note.txt\r\n\r\nUnsupported plain text child\r\n--m--\r\n"
	target := f.add(t, "parent.eml", raw, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	publication, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "nested-publication"))
	require.NoError(t, err)
	require.Len(t, publication.Relations, 2)
	childVersion, err := f.catalog.ContentVersionByID(t.Context(), publication.Relations[0].Child.VersionID)
	require.NoError(t, err)
	childView, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, store.EmailTarget{Version: childVersion})
	require.NoError(t, err)
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic child receipt test")
	var rendered bytes.Buffer
	require.NoError(t, pdf.Output(&rendered))
	runtime := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: pdfTestRenderer{rendered.Bytes()}, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "synthetic-child-test", RendererSHA256: strings.Repeat("a", 64), WorkerSHA256: strings.Repeat("b", 64), FontsSHA256: strings.Repeat("c", 64), Paper: "A4"}}
	render, err := runtime.Request(t.Context(), childVersion.ID, childView.Generation.ID, "A4")
	require.NoError(t, err)
	grantWorkerConsent(t, f.catalog, render)
	job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), render)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: api.NewOperationGate(), Owner: "attachment-export", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	status, err := f.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, status.State)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(render.Profile.CanonicalProfile, &profile))
	recipe, err := canonical.Marshal(profile.Rendition.EmailPDF.Recipe)
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "attachment_original"}, {Role: "attachment_pdf", RecipeSHA256: renditionBytesSHA256(recipe)}}}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	request.OperationID = uuid.NewString()
	request.Roles[1].AllowUnavailable = true
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, 3, plan.RoleEntries)
	validator := bundle.RowValidator{Plan: plan}
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		if d.Attachment != nil {
			if d.Attachment.Order == 1 {
				require.Equal(t, "available", d.Roles[1].Status)
			} else {
				require.Equal(t, "unavailable", d.Roles[1].Status)
				require.NotEmpty(t, d.Roles[1].Reason)
			}
		}
		return validator.Add(d)
	}))
	require.NoError(t, validator.Finish())
	preview, err := f.catalog.ExportPlanPreview(t.Context(), "owner", plan.ID)
	require.NoError(t, err)
	require.Equal(t, 1, preview.Roles[1].AvailableMembers)
	require.Equal(t, 1, preview.Roles[1].UnavailableMembers)
}

func TestExportAttachmentEmptyMissingAndAmbiguousPublication(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "empty.eml", "Subject: Empty\r\nContent-Type: text/plain\r\n\r\nNo attachments.\r\n", "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "attachment_original"}}}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	request.OperationID = uuid.NewString()
	request.Roles[1].AllowUnavailable = true
	missing, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), missing.ID, func(d bundle.Document) error { require.Equal(t, "unavailable", d.Inventory.State); return nil }))
	_, err = PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "empty-first"))
	require.NoError(t, err)
	request.OperationID = uuid.NewString()
	request.Roles[1].AllowUnavailable = false
	empty, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	preview, err := f.catalog.ExportPlanPreview(t.Context(), "owner", empty.ID)
	require.NoError(t, err)
	require.Equal(t, 1, preview.Roles[1].AvailableMembers)
	require.Zero(t, preview.Roles[1].Files)
	_, err = PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "empty-second"))
	require.NoError(t, err)
	request.OperationID = uuid.NewString()
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.ErrorIs(t, err, bundle.ErrConflict)
	request.OperationID = uuid.NewString()
	request.Publications = []bundle.PublicationSelection{{VersionID: target.Version.ID, OperationID: "empty-first"}}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
}

func TestExportAttachmentInventoryLargerThanOneMember(t *testing.T) {
	f := newEmailPipelineFixture(t)
	var sourceText strings.Builder
	sourceText.WriteString("Subject: Many attachments\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nBody\r\n")
	for i := range 200 {
		fmt.Fprintf(&sourceText, "--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=repeat.txt\r\n\r\nSynthetic attachment %d\r\n", i)
	}
	sourceText.WriteString("--m--\r\n")
	target := f.add(t, "many.eml", sourceText.String(), "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	publication, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "many-parts"))
	require.NoError(t, err)
	require.Len(t, publication.Relations, 200)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "attachment_original"}}})
	require.NoError(t, err)
	require.Equal(t, 201, plan.Rows())
	require.Equal(t, 200, plan.RoleEntries)
	validator := bundle.RowValidator{Plan: plan}
	var total int
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		raw, e := json.Marshal(d)
		require.NoError(t, e)
		require.Less(t, len(raw), bundle.MaxMemberBytes)
		total += len(raw)
		return validator.Add(d)
	}))
	require.Greater(t, total, bundle.MaxMemberBytes)
	require.NoError(t, validator.Finish())
}
