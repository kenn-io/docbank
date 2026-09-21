package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/report"
)

func termFrameRequest() report.Request {
	return report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{
			{Number: 1, Expression: "alpha", Syntax: "simple", Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}},
			{Number: 2, Expression: "beta", Syntax: "simple", Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}},
		}}
}

func TestTermReportFrozenGenerationAndCurrentVersions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	alpha, err := s.CreateFile(ctx, s.RootID(), "alpha.txt", fakeHash("report-alpha"), 10, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "beta.txt", fakeHash("report-beta"), 10, "text/plain")
	require.NoError(t, err)
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(ctx, termFrameRequest(), report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.Equal(t, s.VaultID(), frame.VaultID)
	require.Equal(t, "native", frame.GenerationKind)
	require.Len(t, frame.Members, 2)
	require.Equal(t, []bool{true, false}, frame.Members[0].RawMatches)
	require.Equal(t, []bool{false, true}, frame.Members[1].RawMatches)
	for _, member := range frame.Members {
		require.NotEmpty(t, member.Identity.VersionID)
		require.Len(t, member.Identity.SHA256, 64)
		require.NotEmpty(t, member.Candidates, "addition evidence must be retained")
	}
	_, _, err = s.ReplaceContent(ctx, alpha.ID, alpha.Revision, fakeHash("report-alpha-replaced"), 11, "text/plain")
	require.NoError(t, err)
	require.Equal(t, []bool{true, false}, frame.Members[0].RawMatches, "frozen bits changed after a live replacement")
	fresh, err := s.MaterializeTermReportFrame(ctx, termFrameRequest(), report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.NotEqual(t, frame.Members[0].Identity.VersionID, fresh.Members[0].Identity.VersionID)
}

func TestTermReportScopeAndEarliestAddition(t *testing.T) {
	s := newTestStore(t)
	first := createCollectionRun(t, s, "alpha.txt", "term-scope-alpha")
	_ = createCollectionRun(t, s, "beta.txt", "term-scope-beta")
	ctx := context.Background()
	request := termFrameRequest()
	request.AllDocuments = false
	request.CollectionIDs = []string{first.ID()}
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(ctx, request, report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.Len(t, frame.Members, 1)
	require.Equal(t, []bool{true, false}, frame.Members[0].RawMatches)
	require.Len(t, frame.Members[0].CollectionWitnesses, 1)
	require.Equal(t, first.ID(), frame.Members[0].CollectionWitnesses[0].CollectionID)
	require.Len(t, frame.Members[0].CollectionWitnesses[0].MembershipSHA256, 64)
	request.CollectionIDs = []string{"unknown"}
	_, err = s.MaterializeTermReportFrame(ctx, request, report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.Error(t, err)
}

func TestTermReportNativeTextProducesFrozenContentDate(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	digest := sha256.Sum256([]byte("term-native"))
	hash := hex.EncodeToString(digest[:])
	_, err := s.CreateFile(ctx, s.RootID(), "alpha.txt", hash, 24, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: hash, Extractor: "synthetic-native", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "Document dated 2024-05-06",
	}))
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	svc := &reporting.Service{Source: s, Budget: budget}
	frame, err := svc.Prepare(ctx, termFrameRequest())
	require.NoError(t, err)
	require.Len(t, frame.Texts, 1)
	require.Nil(t, frame.Texts[0].Native.Text)
	require.Equal(t, report.StateComplete, frame.Members[0].Coverage.SearchState)
	require.Len(t, frame.Members[0].Candidates, 2)
	result, err := svc.Finalize(ctx, frame, nil)
	require.NoError(t, err)
	require.Equal(t, "2024-05-06", result.Frame.Members[0].Selection.Date)
	require.Equal(t, int64(1), result.Counts[0].Hits)
}

func TestTermReportSharedChildJoinsExactVersionFamily(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "report-parent.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	first, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "report-family-first"))
	require.NoError(t, err)
	require.Len(t, first.Relations, 1)
	child := first.Relations[0].Child
	require.NotNil(t, child)
	childNode, err := s.NodeByID(t.Context(), child.NodeID)
	require.NoError(t, err)
	other, err := s.CreateFile(t.Context(), s.RootID(), "other-parent.eml", view.Version.BlobHash,
		view.Version.Size, "message/rfc822")
	require.NoError(t, err)
	publication := f.publication
	publication.ContentVersionID = other.CurrentVersionID
	otherView, err := s.PublishEmailGeneration(t.Context(), publication)
	require.NoError(t, err)
	request := attachmentRequest(t, s, otherView, "report-family-shared")
	request.Reuse = []document.EmailDocumentReuse{{PartPath: "1.2", Child: *child, Revision: childNode.Revision}}
	_, err = s.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(t.Context(), termFrameRequest(),
		report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.Len(t, frame.Relations, 2)
	families := make(map[int64]string)
	for _, member := range frame.Members {
		families[member.Identity.NodeID] = member.FamilyID
	}
	require.NotEmpty(t, families[child.NodeID])
	require.Equal(t, families[view.Version.NodeID], families[child.NodeID])
	require.Equal(t, families[other.ID], families[child.NodeID])
}

func TestTermReportRetainsRejectedRawMetadataWithoutEventHead(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source := sha256.Sum256([]byte("raw-date-source"))
	hash := hex.EncodeToString(source[:])
	_, err := s.CreateFile(ctx, s.RootID(), "message.eml", hash, 15, "message/rfc822")
	require.NoError(t, err)
	raw := "not an RFC 5322 date"
	metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{
			Key: "email.sent.raw", Namespace: "email", SourceField: "Date",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &raw},
		}}}
	encoded, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	extractor := sha256.Sum256([]byte("synthetic extractor"))
	_, err = s.PublishSourceMetadata(ctx, hash, hex.EncodeToString(extractor[:]), encoded)
	require.NoError(t, err)
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(ctx, termFrameRequest(),
		report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.Len(t, frame.RawDateFields, 1)
	require.Equal(t, raw, frame.RawDateFields[0].Raw)
	require.Equal(t, "email.sent.raw", frame.RawDateFields[0].Key)
}

func TestTermReportLabelsLegacyMissingAdditionAsRecordedFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "legacy.txt", fakeHash("legacy-date"), 1, "text/plain")
	require.NoError(t, err)
	var earliest string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT recorded_at FROM content_versions WHERE version_id=?`,
		node.CurrentVersionID).Scan(&earliest))
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET created_at='' WHERE id=?`, node.ID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("legacy-date-new"), 2, "text/plain")
	require.NoError(t, err)
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(ctx, termFrameRequest(),
		report.CoverageSelection{Configuration: "unconfigured"}, budget)
	require.NoError(t, err)
	require.Len(t, frame.Members, 1)
	require.Equal(t, "vault_observation", frame.Members[0].Candidates[0].SourceClass)
	require.Equal(t, "vault_recorded", frame.Members[0].Candidates[0].Role)
	require.Equal(t, earliest, frame.Members[0].Candidates[0].Raw)
}

func TestTermReportCapturesExactRenditionBinding(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	generationID := testSHA256([]byte("term-report-rendition-generation"))
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", generationID))
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	frame, err := s.MaterializeTermReportFrame(t.Context(), termFrameRequest(),
		report.CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}, budget)
	require.NoError(t, err)
	require.Equal(t, generationID, frame.GenerationID)
	require.Len(t, frame.Texts, 1)
	binding := frame.Texts[0]
	require.Equal(t, "rendition", binding.Kind)
	require.Equal(t, versions[0], binding.Document.VersionID)
	require.Equal(t, profile.Fingerprint, binding.ProfileFingerprint)
	require.Equal(t, generationID, binding.GenerationID)
	require.Equal(t, attachment.ID, binding.AttachmentID)
	require.Equal(t, build.ID, binding.BuildID)
	require.Equal(t, build.Artifacts[1].ID, binding.ArtifactID)
	require.Equal(t, catalogMarkdownBlobHash, binding.ArtifactSHA256)
	require.Equal(t, int64(len(catalogBlobContents[catalogMarkdownBlobHash])), binding.Size)
	require.Equal(t, report.StateComplete, frame.Members[0].Coverage.SearchState)
}
