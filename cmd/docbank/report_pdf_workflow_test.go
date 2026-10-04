package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func TestReportPDFWorkflow(t *testing.T) {
	f := newPDFReportFixture(t)
	f.start(t, f.root)
	_, packet := f.capture(t, f.request)
	archive, receipt := exportReportOriginals(t, f.originals[:3])
	assertOriginalArchive(t, archive, receipt, f.originals[:3])

	before, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	strict := f.request
	strict.CoverageMode = "strict"
	strict.SelectedDocuments = &report.SelectedDocuments{
		Documents: f.request.SelectedDocuments.Documents[2:3],
	}
	encoded, err := json.Marshal(strict)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "strict.zip")
	_, err = runCLI(t, "search-export", "create", "--input",
		writeSourceFile(t, "strict.json", string(encoded)), "--output", destination)
	requireReportProblem(t, err, "incomplete_coverage")
	require.NoFileExists(t, destination)
	after, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	require.JSONEq(t, before, after, "a refused strict capture must not create history")
	out, err := runCLI(t, "search-export", "verify", packet)
	require.NoError(t, err)
	require.Contains(t, out, "internally consistent: true; source verified: false")
}

func (f *pdfReportFixture) capture(t *testing.T, request report.Request) (report.Summary, string) {
	t.Helper()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	packet := filepath.Join(t.TempDir(), "report.zip")
	out, err := runCLI(t, "search-export", "create", "--input",
		writeSourceFile(t, "request.json", string(encoded)), "--output", packet)
	require.NoError(t, err)
	idPattern := regexp.MustCompile(`\b[0-9a-f]{48}\b`)
	parentID := idPattern.FindString(out)
	require.NotEmpty(t, parentID, out)
	parent := showReport(t, parentID)
	require.Equal(t, "needs_review", parent.State, "%+v", parent)
	require.EqualValues(t, 1, parent.UnresolvedDates)
	require.Empty(t, parent.Counts)
	require.NoFileExists(t, packet)
	_, err = runCLI(t, "search-export", "download", parentID, "--output", packet)
	require.ErrorIs(t, err, daemonconn.ErrReportReviewRequired)
	require.NoFileExists(t, packet)
	out, err = runCLI(t, "search-export", "dates", parentID)
	require.NoError(t, err)
	var dates report.DatePage
	require.NoError(t, json.Unmarshal([]byte(out), &dates))
	require.Len(t, dates.Members, 3)
	require.Empty(t, dates.NextCursor)
	var choice report.DateChoice
	for _, member := range dates.Members {
		require.True(t, member.CandidatesComplete)
		switch member.Document.NodeID {
		case f.originals[0].member.NodeID:
			require.Equal(t, "2024-05-06", member.Selection.Date)
		case f.originals[1].member.NodeID:
			for _, candidate := range member.Candidates {
				if candidate.Value == "2024-05-06" && candidate.Role == "document_date" {
					choice = report.DateChoice{Document: member.Document, CandidateID: candidate.ID,
						EvidenceSHA256: candidate.Locator.EvidenceSHA256, Action: "select",
						Reason: "Reviewed synthetic dated source"}
				}
			}
		case f.originals[2].member.NodeID:
			require.Equal(t, "vault_addition", member.Selection.Reason)
			require.GreaterOrEqual(t, member.Selection.Date, "2000-01-01")
			require.LessOrEqual(t, member.Selection.Date, "2099-12-31")
		}
	}
	require.NotEmpty(t, choice.CandidateID)
	encoded, err = json.Marshal([]report.DateChoice{choice})
	require.NoError(t, err)
	out, err = runCLI(t, "search-export", "revise", parentID, "--choices",
		writeSourceFile(t, "choices.json", string(encoded)), "--output", packet)
	require.NoError(t, err)
	child := showReport(t, idPattern.FindString(out))
	require.Equal(t, parentID, child.ParentID)
	require.Equal(t, parent.ExpiresAt, child.ExpiresAt)
	require.Equal(t, parent, showReport(t, parentID), "review must leave the parent unchanged")
	f.assertReport(t, child, packet)
	return child, packet
}

func showReport(t *testing.T, id string) report.Summary {
	t.Helper()
	out, err := runCLI(t, "search-export", "show", id, "--json")
	require.NoError(t, err)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	return summary
}

func requireReportProblem(t *testing.T, err error, want string) {
	t.Helper()
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, want, code, "%v", err)
}

func (f *pdfReportFixture) assertReport(t *testing.T, summary report.Summary, packet string) {
	t.Helper()
	require.Equal(t, "complete", summary.State)
	require.Equal(t, []report.Counts{
		{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
		{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
	}, summary.Counts)
	require.Len(t, summary.RowCoverage, 2)
	for _, coverage := range append([]report.Coverage{summary.Coverage}, summary.RowCoverage...) {
		require.EqualValues(t, 3, coverage.Scoped)
		require.EqualValues(t, 2, coverage.Searchable)
		require.EqualValues(t, 1, coverage.MissingText)
		require.Zero(t, coverage.IncompleteFamilies)
		require.EqualValues(t, 1, coverage.FallbackDates)
	}
	out, err := runCLI(t, "search-export", "verify", packet)
	require.NoError(t, err)
	require.Contains(t, out, "internally consistent: true; source verified: false")
	z, err := zip.OpenReader(packet)
	require.NoError(t, err)
	defer func() { require.NoError(t, z.Close()) }()
	stream, err := z.Open("members.jsonl")
	require.NoError(t, err)
	content, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	var identities []report.Identity
	for line := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		var member report.Member
		require.NoError(t, json.Unmarshal([]byte(line), &member))
		identities = append(identities, member.Identity)
		if member.Identity == f.request.SelectedDocuments.Documents[2] {
			require.Equal(t, "missing", member.Coverage.SearchState)
			require.Equal(t, []bool{false, false}, member.RawMatches)
		}
	}
	require.ElementsMatch(t, f.request.SelectedDocuments.Documents, identities)
	csvPath := filepath.Join(t.TempDir(), "counts.csv")
	_, err = runCLI(t, "search-export", "csv", packet, "--output", csvPath)
	require.NoError(t, err)
	content, err = os.ReadFile(csvPath)
	require.NoError(t, err)
	rows, err := csv.NewReader(bytes.NewReader(content)).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"Term #", "Terms", "Date Range", "Hits", "Hits Plus Family",
			"Unique Hits", "Unique Families", "Unique Hits Plus Family"},
		{"1", "alpha", "2000-01-01 to 2099-12-31", "1", "1", "1", "1", "1"},
		{"2", "beta", "2000-01-01 to 2099-12-31", "1", "1", "1", "1", "1"},
	}, rows)
}

func exportReportOriginals(t *testing.T, originals []reportOriginal) (string, bundle.Receipt) {
	t.Helper()
	request := exportPreviewRequest{
		SourceOperationID: uuid.New().String(), PlanOperationID: uuid.New().String(),
	}
	for _, original := range originals {
		request.Members = append(request.Members, original.member)
	}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	out, err := runCLI(t, "export", "preview", "--request",
		writeSourceFile(t, "selection.json", string(encoded)), "--json")
	require.NoError(t, err)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(out), &plan))
	require.Equal(t, len(originals), plan.Total)
	require.Equal(t, []bundle.RolePolicy{{Role: "original"}}, plan.Roles)
	jobID := uuid.New().String()
	_, err = runCLI(t, "export", "start", plan.ID, "--fingerprint", plan.Fingerprint,
		"--operation-id", jobID, "--json")
	require.NoError(t, err)
	var job bundle.Job
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "export", "status", jobID, "--json")
		if err == nil {
			err = json.Unmarshal([]byte(out), &job)
		}
		return err != nil || job.State == "completed" || job.State == "failed" || job.State == "canceled"
	}, 30*time.Second, 25*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "completed", job.State, job.Failure)
	path := filepath.Join(t.TempDir(), "originals.zip")
	out, err = runCLI(t, "export", "download", jobID, path, "--json")
	require.NoError(t, err)
	var receipt bundle.Receipt
	require.NoError(t, json.Unmarshal([]byte(out), &receipt))
	require.Equal(t, plan.Fingerprint, receipt.PlanFingerprint)
	require.NotNil(t, job.Receipt)
	require.Equal(t, *job.Receipt, receipt)
	require.Eventually(t, func() bool {
		_, err = runCLI(t, "export", "release", jobID)
		code, _ := daemonconn.ProblemCode(err)
		return code != "export_retained"
	}, 30*time.Second, 25*time.Millisecond)
	require.NoError(t, err)
	_, err = runCLI(t, "export", "status", jobID)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	return path, receipt
}

func assertOriginalArchive(
	t *testing.T, path string, receipt bundle.Receipt, originals []reportOriginal,
) {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	verified, err := bundle.Verify(t.Context(), bytes.NewReader(content),
		int64(len(content)), receipt.PlanFingerprint)
	require.NoError(t, err)
	require.Equal(t, receipt, verified)
	z, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err)
	stream, err := z.Open("bundle.json")
	require.NoError(t, err)
	var manifest struct {
		Documents []bundle.Document `json:"documents"`
	}
	require.NoError(t, json.UnmarshalRead(stream, &manifest))
	require.NoError(t, stream.Close())
	require.Len(t, manifest.Documents, len(originals))
	wantNames := []string{"bundle.json", "metadata.csv", "SHA256SUMS"}
	for i, original := range originals {
		doc := manifest.Documents[i]
		require.Equal(t, original.member, doc.Member)
		require.Len(t, doc.Roles, 1)
		require.Equal(t, "original", doc.Roles[0].Role)
		require.Equal(t, "available", doc.Roles[0].Status)
		require.Nil(t, doc.Attachment, "explicit selections are ordinary document rows")
		name := fmt.Sprintf("documents/%d/%s/original", doc.NodeID, doc.VersionID)
		wantNames = append(wantNames, name)
		stream, err := z.Open(name)
		require.NoError(t, err)
		actual, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		require.Equal(t, original.bytes, actual)
		require.Equal(t, original.member.SHA256, sha256HexBytes(actual))
		require.Equal(t, original.member.Size, int64(len(actual)))
	}
	var names []string
	for _, file := range z.File {
		names = append(names, file.Name)
	}
	require.ElementsMatch(t, wantNames, names)
}

func reportHistory(t *testing.T) store.TermReportHistoryPage {
	t.Helper()
	out, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	var history store.TermReportHistoryPage
	require.NoError(t, json.Unmarshal([]byte(out), &history))
	return history
}
