package mcp

import (
	"archive/zip"
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func TestMCPReportAndNativeExportWorkflow(t *testing.T) {
	f := newNativeExportFixture(t)
	contents := []string{"Alpha. Document dated 2024-05-06. Document dated 2024-06-07.",
		"Alpha. Document dated 2024-05-06."}
	first := f.addFile(t, "first.txt", contents[0])
	second := f.addFile(t, "second.txt", contents[1])
	f.addFile(t, "excluded.txt", "Alpha. Document dated 2024-05-06.")
	originals := []bundle.Member{first, second}
	for i, member := range originals {
		require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: member.SHA256, Extractor: "synthetic-native", ExtractorVersion: 1,
			Status: store.ExtractionOK, Text: contents[i],
		}))
	}
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowReportWrites: true, AllowExportWrites: true}, f.lease)
	var identities []report.Identity
	for _, member := range originals {
		versions := exportCall(t, server, "list_document_versions", map[string]any{"node_id": member.NodeID})
		raw, err := json.Marshal(versions)
		require.NoError(t, err)
		var listed listDocumentVersionsOutput
		require.NoError(t, json.Unmarshal(raw, &listed))
		require.Len(t, listed.Items, 1)
		version := listed.Items[0]
		require.Equal(t, member.VersionID, version.ContentVersionID)
		require.Equal(t, member.SHA256, version.BlobHash)
		identities = append(identities, report.Identity{NodeID: member.NodeID,
			VersionID: version.ContentVersionID, SHA256: version.BlobHash})
	}
	request := reportArguments()
	request["selected_documents"] = report.SelectedDocuments{Documents: identities}
	receipt := exportCall(t, server, "create_report", map[string]any{"request": request})
	require.Equal(t, "needs_review", receipt["state"], "%+v", receipt)
	id, ok := receipt["report_id"].(string)
	require.True(t, ok)
	dates := exportCall(t, server, "get_report_dates", map[string]any{"report_id": id})
	encoded, err := json.Marshal(dates["page"])
	require.NoError(t, err)
	var page report.DatePage
	require.NoError(t, json.Unmarshal(encoded, &page))
	require.Len(t, page.Members, 2)
	var choice report.DateChoice
	for _, candidate := range page.Members[0].Candidates {
		if candidate.Value == "2024-05-06" && candidate.Role == "document_date" {
			choice = report.DateChoice{Document: candidate.Document, CandidateID: candidate.ID,
				EvidenceSHA256: candidate.Locator.EvidenceSHA256, Action: "select", Reason: "Reviewed dated source"}
			break
		}
	}
	require.NotEmpty(t, choice.CandidateID)
	child := exportCall(t, server, "revise_report", map[string]any{
		"report_id": id, "choices": []report.DateChoice{choice},
	})
	require.Equal(t, "complete", child["state"], "%+v", child)
	require.Equal(t, id, child["parent_id"])
	require.Equal(t, receipt["expires_at"], child["expires_at"])
	childID, ok := child["report_id"].(string)
	require.True(t, ok)
	summary := exportCall(t, server, "get_report_summary", map[string]any{"report_id": childID})
	require.Contains(t, summary, "summary")
	destination := filepath.Join(t.TempDir(), "report.zip")
	output := exportCall(t, server, "download_report", map[string]any{
		"report_id": childID, "destination_path": destination,
	})
	require.Equal(t, "published", output["state"])
	packet, err := os.ReadFile(destination)
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(packet), int64(len(packet)))
	require.NoError(t, err)
	members, err := archive.Open("members.jsonl")
	require.NoError(t, err)
	rawMembers, err := io.ReadAll(members)
	require.NoError(t, err)
	require.NoError(t, members.Close())
	var captured []report.Identity
	for line := range strings.SplitSeq(strings.TrimSpace(string(rawMembers)), "\n") {
		var member struct {
			Identity report.Identity `json:"identity"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &member))
		captured = append(captured, member.Identity)
	}
	require.Equal(t, identities, captured)
	manifest, err := archive.Open("manifest.json")
	require.NoError(t, err)
	var sealed struct {
		Counts []report.Counts `json:"counts"`
	}
	require.NoError(t, json.UnmarshalRead(manifest, &sealed))
	require.NoError(t, manifest.Close())
	require.Equal(t, []report.Counts{{Hits: 2, HitsPlusFamily: 2, UniqueHits: 2,
		UniqueFamilies: 2, UniqueHitsPlusFamily: 2}}, sealed.Counts)

	plan := previewNativeExport(t, server, originals)
	jobID := startNativeExport(t, server, plan)
	worked, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	originalsPath := filepath.Join(t.TempDir(), "originals.zip")
	require.Equal(t, "published", exportCall(t, server, "download_export", map[string]any{
		"job_id": jobID, "destination_path": originalsPath,
	})["state"])
	originalsZIP, err := zip.OpenReader(originalsPath)
	require.NoError(t, err)
	var savedOriginals []string
	for _, entry := range originalsZIP.File {
		if filepath.Base(entry.Name) != "original" {
			continue
		}
		stream, err := entry.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		savedOriginals = append(savedOriginals, string(content))
	}
	require.NoError(t, originalsZIP.Close())
	require.ElementsMatch(t, contents, savedOriginals)

	node, err := f.catalog.NodeByID(t.Context(), first.NodeID)
	require.NoError(t, err)
	hash, size, err := f.blobs.Write(strings.NewReader("replacement without the selected evidence"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), first.NodeID, node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	node, err = f.catalog.NodeByID(t.Context(), second.NodeID)
	require.NoError(t, err)
	_, _, err = f.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	stale := exportCall(t, server, "create_report", map[string]any{"request": request})
	require.Equal(t, "report_selection_changed", stale["code"])
	require.Equal(t, summary, exportCall(t, server, "get_report_summary", map[string]any{"report_id": childID}))
	again := filepath.Join(t.TempDir(), "frozen.zip")
	require.Equal(t, "published", exportCall(t, server, "download_report", map[string]any{
		"report_id": childID, "destination_path": again,
	})["state"])
	unchanged, err := os.ReadFile(again)
	require.NoError(t, err)
	require.Equal(t, packet, unchanged)
}

func TestMCPReportAvailableOnlyKeepsMissingEvidence(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "unprocessed.txt", "alpha hidden in unprocessed original")
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true}, f.lease)
	request := report.Request{Version: 1, SelectedDocuments: &report.SelectedDocuments{
		Documents: []report.Identity{{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256}}},
		Timezone: "UTC", CoverageMode: "strict", Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}}}
	strict := exportCall(t, server, "create_report", map[string]any{"request": request})
	require.Equal(t, "incomplete_coverage", strict["code"])
	request.CoverageMode = "available_only"
	available := exportCall(t, server, "create_report", map[string]any{"request": request})
	require.Equal(t, "complete", available["state"])
	output := exportCall(t, server, "get_report_summary", map[string]any{"report_id": available["report_id"]})
	encoded, err := json.Marshal(output["summary"])
	require.NoError(t, err)
	var summary report.Summary
	require.NoError(t, json.Unmarshal(encoded, &summary))
	require.EqualValues(t, 1, summary.Coverage.Scoped)
	require.EqualValues(t, 1, summary.Coverage.MissingText)
	require.EqualValues(t, 0, summary.Counts[0].Hits)
}
