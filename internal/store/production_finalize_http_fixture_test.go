package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

// ProductionFinalizeHTTPFixture has fully reviewed synthetic members and a
// passing retained gate, but leaves finalization to the public command.
func ProductionFinalizeHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string) {
	t.Helper()
	s, first, second, setID, revision, authority := productionDuplicateGateFixture(t)
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), setID, revision, draft.ETag, namespace.NamespaceID
}

// ProductionRenderDaemonHTTPFixture supplies a reviewed, finalizable vault
// with a real synthetic PDF that the daemon renderer can open after restart.
func ProductionRenderDaemonHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string) {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 72, Ht: 72}})
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(20, 36, "x")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	pdfBytes := output.Bytes()
	s, first, second, setID, revision, authority := productionDuplicateGateFixtureWithPDF(t,
		pdfBytes, redaction.Box{X0: 0, Y0: 0, X1: 10_000, Y1: 10_000})
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	root := filepath.Dir(s.path)
	f := &realRestartFixture{Store: s, root: root}
	f.blobs, err = openRestartBlobs(s, root)
	require.NoError(t, err)
	written, err := f.write(t.Context(), bytes.NewReader(pdfBytes))
	require.NoError(t, err)
	require.Equal(t, first.PDFSHA256, written.Hash)
	require.NoError(t, f.blobs.Close())
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	return s, root, setID, revision, draft.ETag, namespace.NamespaceID
}

// ProductionRequiredApprovalHTTPFixture has reviewed synthetic inputs and an
// exact authenticated-human grant, but leaves selection and finalization to
// the public API under test.
func ProductionRequiredApprovalHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string, string) {
	t.Helper()
	fixture := newProductionPolicyFinalizationFixture(t, true, false)
	stored := loadProductionInputsForTest(t, fixture.store, fixture.set.ID, fixture.draft.Revision)
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	now := time.Now().UTC()
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "76000000-0000-4000-8000-000000000022",
		ApprovalID:  "76000000-0000-4000-8000-000000000023",
		Subject:     subject, Evidence: "Synthetic approval evidence.",
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: now.Format(time.RFC3339Nano), EvidenceSHA256: fakeHash("e5"),
		},
	}, now)
	require.NoError(t, err)
	grant, err := fixture.store.PutProductionApproval(t.Context(), record)
	require.NoError(t, err)
	return fixture.store, filepath.Dir(fixture.store.path), fixture.set.ID, fixture.draft.Revision,
		fixture.current.ETag, fixture.namespaceID, grant.ID
}
