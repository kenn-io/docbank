package docbank

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/docbank/internal/misslog"
	"strings"
	"testing"
	"time"
	"uuid"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// Receipt assertions use an idle vault; contention has separate coverage.
func stopMissLogBackgroundWrites(vault *Vault) {
	vault.processingCancel()
	vault.processingWG.Wait()
}

func TestEmbeddedMissLogDoesNotWaitForBusyWrite(t *testing.T) {
	t.Parallel()
	for _, maintenance := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary write", true: "maintenance"}[maintenance], func(t *testing.T) {
			vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			stopMissLogBackgroundWrites(vault)
			id, err := uuid.Parse(vault.ID())
			require.NoError(t, err)
			args, err := json.Marshal(map[string]any{"query": "synthetic busy miss", "profile": "supplied-transcript", "mode": "lexical", "filters": map[string]any{}})
			require.NoError(t, err)
			if maintenance {
				vault.preservation.Lock()
				defer vault.preservation.Unlock()
			}
			vault.mutation.Lock()
			defer vault.mutation.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			type outcome struct {
				result *sdkmcp.CallToolResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := vault.CallReadTool(ctx, id, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: args}})
				done <- outcome{result, err}
			}()
			select {
			case result := <-done:
				require.NoError(t, result.err)
				require.False(t, result.result.IsError)
			case <-ctx.Done():
				t.Fatal("an embedded search must not wait for best-effort logging")
			}
		})
	}
}

func TestEmbeddedMissLogUsesOwnedVaultAndEmptyScope(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: enabled}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
		stopMissLogBackgroundWrites(vault)
		id, err := uuid.Parse(vault.ID())
		require.NoError(t, err)
		args, err := json.Marshal(map[string]any{"query": "synthetic embedded miss", "profile": "supplied-transcript", "mode": "lexical", "filters": map[string]any{}})
		require.NoError(t, err)
		result, err := vault.CallReadTool(t.Context(), id, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: args}})
		require.NoError(t, err)
		require.False(t, result.IsError, result)
		rows, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
		require.NoError(t, err)
		if enabled {
			require.Len(t, rows, 1)
			require.Contains(t, rows[0].Receipt, "synthetic embedded miss")
		} else {
			require.Empty(t, rows)
		}
		require.NoError(t, vault.Close())
	}
}

func TestEmbeddedMissLogDirectDocumentSearch(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	stopMissLogBackgroundWrites(vault)
	receipt, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("ordinary fixture content"), PutOptions{})
	require.NoError(t, err)
	report, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: "unfindablequestionzz", Mode: DocumentSearchLexical, Profile: "supplied-transcript", Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}})
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.NotEmpty(t, report.MissLogID)
	require.Len(t, report.MissLogCapability, 64)
	records, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Receipt, "unfindablequestionzz")
}

func TestEmbeddedOwnerFeedbackRemainsExplicitAndWorksWhenRecordingDisabled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root, MissLog: MissLogOptions{Enabled: true}})
	require.NoError(t, err)
	put, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("ordinary synthetic content"), PutOptions{})
	require.NoError(t, err)
	stopMissLogBackgroundWrites(vault)
	report, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: "synthetic owner feedback", Mode: DocumentSearchLexical, Profile: "supplied-transcript", Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{put.Version.ID}}})
	require.NoError(t, err)
	require.NoError(t, vault.RecordSearchFeedback(t.Context(), report.MissLogID, SearchFeedback{Verdict: "wrong"}))
	require.ErrorIs(t, vault.RecordSearchFeedback(t.Context(), strings.Repeat("a", 48), SearchFeedback{Verdict: "wrong"}), ErrSearchReceiptUnavailable)
	require.NoError(t, vault.Close())
	vault, err = New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	defer vault.Close()
	require.NoError(t, vault.RecordSearchFeedback(t.Context(), report.MissLogID, SearchFeedback{Verdict: "none_in_vault"}))
	rows, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Receipt, `"verdict":"none_in_vault"`)
}

func TestEmbeddedOwnerFeedbackCachesDocumentHits(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
	require.NoError(t, err)
	defer vault.Close()
	stopMissLogBackgroundWrites(vault)
	put, err := vault.Put(t.Context(), "/synthetic-hit.txt", strings.NewReader("ordinary private content"), PutOptions{})
	require.NoError(t, err)
	report, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: "synthetic", Mode: DocumentSearchLexical, Profile: "supplied-transcript", Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{put.Version.ID}}})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Len(t, report.MissLogID, 48)
	require.Len(t, report.MissLogCapability, 64)
	rows, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, vault.RecordSearchFeedback(t.Context(), report.MissLogID, SearchFeedback{Verdict: "low_confidence", Expected: []SearchFeedbackIdentity{{NodeID: put.Node.ID}}}))
	rows, err = vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotContains(t, rows[0].Receipt, `"excerpt"`)
	require.NotContains(t, rows[0].Receipt, report.MissLogCapability)
}

func TestEmbeddedReceiptKeepsBoundedEvidenceWhenMediaReferenceIsWide(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	stopMissLogBackgroundWrites(vault)
	put, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("synthetic content"), PutOptions{})
	require.NoError(t, err)
	short := DocumentEvidenceReference{Kind: "lexical_segment", BuildID: strings.Repeat("a", 64), SegmentID: strings.Repeat("b", 64)}
	wide := short
	for i := range 6 {
		wide.MediaSources = append(wide.MediaSources, DocumentMediaSource{SourceID: fmt.Sprintf("%064x", i+1), SourceVersionID: fmt.Sprintf("%064x", i+100), ContentVersionID: put.Version.ID})
	}
	encodedWide, err := json.Marshal(wide, json.Deterministic(true))
	require.NoError(t, err)
	require.Greater(t, len(encodedWide), 1024)
	report := DocumentSearchReport{RequestedMode: DocumentSearchLexical, ActualMode: DocumentSearchLexical, Results: []DocumentSearchResult{{NodeID: put.Node.ID, ContentVersionID: put.Version.ID, Rank: 1, Score: 0.75, Path: "/synthetic.txt", Evidence: []DocumentEvidenceReference{wide, short}}}}
	request := DocumentSearchRequest{Query: "synthetic", Mode: DocumentSearchLexical, Profile: "supplied-transcript", Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{put.Version.ID}}}
	vault.recordSearchReceipt(t.Context(), request, &report)
	require.Len(t, report.MissLogID, 48)
	require.NoError(t, vault.RecordSearchFeedback(t.Context(), report.MissLogID, SearchFeedback{Verdict: "wrong"}))
	row, err := vault.metadata.MissLogRecordByID(t.Context(), report.MissLogID)
	require.NoError(t, err)
	var receipt misslog.Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &receipt))
	require.Len(t, receipt.Hits, 1)
	require.Len(t, receipt.Hits[0].EvidenceIDs, 1)
	var retained DocumentEvidenceReference
	require.NoError(t, json.Unmarshal([]byte(receipt.Hits[0].EvidenceIDs[0]), &retained))
	require.Equal(t, short, retained)
}
