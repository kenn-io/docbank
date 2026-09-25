package processing

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

func TestExportEmailPDFMatchesSelectedAttachmentGeneration(t *testing.T) {
	t.Parallel()
	f := newEmailPipelineFixture(t)
	target := f.add(t, "synthetic.eml", emailPipelineSource, "message/rfc822")
	current, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	// A toolchain change gives the same source a different decoder recipe.
	// Retain a validated historical recipe without running an older decoder.
	historicalEvidence := current.Evidence
	historicalEvidence.Recipe.GoVersion += "-historical-test"
	raw, _, err := document.MarshalEmailV1(historicalEvidence)
	require.NoError(t, err)
	historical, err := f.catalog.PublishEmailGeneration(t.Context(), store.EmailPublication{ContentVersionID: target.Version.ID, CanonicalJSON: raw, Artifacts: current.Generation.Artifacts})
	require.NoError(t, err)
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic generation export")
	var rendered bytes.Buffer
	require.NoError(t, pdf.Output(&rendered))
	runtime := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: pdfTestRenderer{rendered.Bytes()}, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "synthetic-test", RendererSHA256: strings.Repeat("a", 64), WorkerSHA256: strings.Repeat("b", 64), FontsSHA256: strings.Repeat("c", 64), BubblewrapSHA256: strings.Repeat("d", 64), Paper: "A4"}}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: newTestOperationGate(), Owner: "generation-export", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	render := func(generation string) string {
		request, err := runtime.Request(t.Context(), target.Version.ID, generation, "A4")
		require.NoError(t, err)
		grantWorkerConsent(t, f.catalog, request)
		job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		_, err = worker.RunJob(t.Context(), job.ID)
		require.NoError(t, err)
		status, err := f.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, status.State)
		return request.Profile.Fingerprint
	}
	historicalProfile := render(historical.Generation.ID)
	current, err = EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.NotEqual(t, historical.Generation.ID, current.Generation.ID)
	publication, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, current, "current-inventory"))
	require.NoError(t, err)
	historicalPublication, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, historical, "historical-inventory"))
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(runtime.Recipe)
	require.NoError(t, err)
	for _, policy := range []bundle.RolePolicy{
		{Role: "email_pdf", RecipeSHA256: renditionBytesSHA256(recipe)},
		{Role: "email_pdf", ProfileFingerprint: historicalProfile},
	} {
		request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{policy, {Role: "attachment_original"}}, Publications: []bundle.PublicationSelection{{VersionID: target.Version.ID, OperationID: publication.OperationID}}}
		_, err = f.catalog.CreateExportPlan(t.Context(), "owner", request)
		require.ErrorIs(t, err, bundle.ErrUnavailable)
		request.OperationID = uuid.NewString()
		request.Roles[0].AllowUnavailable = true
		plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", request)
		require.NoError(t, err)
		require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
			if d.Attachment == nil {
				require.Equal(t, "unavailable", d.Roles[0].Status)
				require.Equal(t, current.Generation.ID, d.Inventory.GenerationID)
			}
			return nil
		}))
	}
	currentProfile := render(current.Generation.ID)
	for _, selection := range []struct{ operation, generation, profile string }{
		{publication.OperationID, current.Generation.ID, currentProfile},
		{historicalPublication.OperationID, historical.Generation.ID, historicalProfile},
	} {
		for _, policy := range []bundle.RolePolicy{
			{Role: "email_pdf", RecipeSHA256: renditionBytesSHA256(recipe)},
			{Role: "email_pdf", ProfileFingerprint: selection.profile},
		} {
			plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{policy, {Role: "attachment_original"}}, Publications: []bundle.PublicationSelection{{VersionID: target.Version.ID, OperationID: selection.operation}}})
			require.NoError(t, err)
			require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
				if d.Attachment == nil {
					var receipt document.EmailPDFReceiptV1
					require.NoError(t, json.Unmarshal(d.Roles[0].Recipe, &receipt))
					require.Equal(t, selection.generation, receipt.Binding.GenerationID)
				}
				return nil
			}))
		}
	}
	_, err = f.catalog.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "email_pdf", RecipeSHA256: renditionBytesSHA256(recipe)}}})
	require.ErrorIs(t, err, bundle.ErrConflict, "without an attachment selection, two PDF generations remain ambiguous")
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
}
