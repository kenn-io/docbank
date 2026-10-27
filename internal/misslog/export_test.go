package misslog

import (
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func exportReceipt(t *testing.T, svc *Service, st *store.Store, receipt Receipt) string {
	t.Helper()
	receipt.ID = strings.Repeat("a", 40) + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	receipt.CreatedAt = time.Now().UTC()
	receipt.VaultUID = st.VaultID()
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, time.Now(), 90, 5000))
	return receipt.ID
}

func TestExportEligibilityAndMatchingEvidence(t *testing.T) {
	svc, st := feedbackService(t)
	node, err := st.CreateFile(t.Context(), st.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	fence := processing.SourceFence{VaultUID: st.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}
	fp, err := processing.SourceFenceFingerprint(fence)
	require.NoError(t, err)
	scope := map[string]any{"fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}}
	positive := Receipt{Query: "synthetic question", Scope: scope, FenceFingerprint: fp, Feedback: &Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: node.ID}}}, Hits: []Hit{
		{Rank: 1, NodeID: node.ID, ContentVersionID: node.CurrentVersionID, EvidenceIDs: []string{"matching", "matching"}},
		{Rank: 2, NodeID: node.ID, ContentVersionID: "other-version", EvidenceIDs: []string{"unrelated"}},
	}}
	positiveID := exportReceipt(t, svc, st, positive)
	noneID := exportReceipt(t, svc, st, Receipt{Query: "synthetic missing", Feedback: &Feedback{Verdict: "none_in_vault"}})
	exportReceipt(t, svc, st, Receipt{Query: "unlabeled private query"})
	exportReceipt(t, svc, st, Receipt{Query: "unknown answer", Feedback: &Feedback{Verdict: "low_confidence"}})
	live := positive
	live.Scope = nil
	live.FenceFingerprint = ""
	exportReceipt(t, svc, st, live)
	page, err := svc.Export(t.Context(), "eval", 100, 0)
	require.NoError(t, err)
	require.True(t, page.Done)
	require.Len(t, page.Items, 2)
	require.Equal(t, 1, page.Skipped["unlabeled"])
	require.Equal(t, 1, page.Skipped["missing_expected"])
	require.Equal(t, 1, page.Skipped["immutable_scope_required"])
	for _, item := range page.Items {
		if item.SourceRecordID == positiveID {
			require.Equal(t, []Identity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}}, item.Expected)
			require.Contains(t, item.Scope, node.CurrentVersionID)
		} else {
			require.Equal(t, noneID, item.SourceRecordID)
			require.Empty(t, item.Expected)
			require.Empty(t, item.Scope)
		}
	}
	cards, err := svc.Export(t.Context(), "cards", 100, 0)
	require.NoError(t, err)
	require.Len(t, cards.Items, 3)
	for _, item := range cards.Items {
		if item.SourceRecordID == positiveID {
			require.Equal(t, []string{"matching"}, item.EvidenceIDs)
		}
	}
	_, _, err = st.ReplaceContent(t.Context(), node.ID, node.Revision, strings.Repeat("b", 64), 1, "text/plain")
	require.NoError(t, err)
	page, err = svc.Export(t.Context(), "eval", 100, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, 1, page.Skipped["outside_recorded_scope"])
}

func TestExportRejectsInvalidScopeAndDeletedIdentity(t *testing.T) {
	svc, st := feedbackService(t)
	node, err := st.CreateFile(t.Context(), st.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	receipt := Receipt{Query: "synthetic", Feedback: &Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}}}, Scope: map[string]any{"fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}}, FenceFingerprint: "tampered"}
	exportReceipt(t, svc, st, receipt)
	page, err := svc.Export(t.Context(), "eval", 100, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Skipped["invalid_scope"])
	_, _, err = st.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	page, err = svc.Export(t.Context(), "cards", 100, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Skipped["identity_unavailable"])
	for _, args := range []struct {
		format        string
		limit, offset int
	}{{"invalid", 1, 0}, {"eval", 0, 0}, {"cards", 101, 0}, {"eval", 1, -1}} {
		_, err := svc.Export(t.Context(), args.format, args.limit, args.offset)
		require.Error(t, err)
	}
}

func TestExportBoundsReceiptAndPageBytes(t *testing.T) {
	svc, st := feedbackService(t)
	for range 3 {
		exportReceipt(t, svc, st, Receipt{Query: strings.Repeat("q", 32768), Scope: map[string]any{"live": strings.Repeat("s", 3<<20)}, Feedback: &Feedback{Verdict: "none_in_vault"}})
	}
	rows, more, err := st.MissLogExportRecords(t.Context(), 100, 0)
	require.NoError(t, err)
	require.True(t, more)
	require.Len(t, rows, 2)
	page, err := svc.Export(t.Context(), "eval", 100, 0)
	require.NoError(t, err)
	require.False(t, page.Done)
	require.Greater(t, page.NextOffset, 0)
	require.Less(t, page.NextOffset, 3)
	next, err := svc.Export(t.Context(), "eval", 100, page.NextOffset)
	require.NoError(t, err)
	require.True(t, next.Done)
	require.Len(t, append(page.Items, next.Items...), 3)
}

func TestExportBoundsProjectedPagesAndExplicitOldVersions(t *testing.T) {
	svc, st := feedbackService(t)
	for range 40 {
		exportReceipt(t, svc, st, Receipt{Query: strings.Repeat("q", 32768), Feedback: &Feedback{Verdict: "none_in_vault"}})
	}
	page, err := svc.Export(t.Context(), "cards", 100, 0)
	require.NoError(t, err)
	require.False(t, page.Done)
	raw, err := json.Marshal(page.Items)
	require.NoError(t, err)
	require.Less(t, len(raw), 1<<20)
	next, err := svc.Export(t.Context(), "cards", 100, page.NextOffset)
	require.NoError(t, err)
	require.True(t, next.Done)
	require.Len(t, append(page.Items, next.Items...), 40)
	node, err := st.CreateFile(t.Context(), st.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	old := node.CurrentVersionID
	_, _, err = st.ReplaceContent(t.Context(), node.ID, node.Revision, strings.Repeat("b", 64), 1, "text/plain")
	require.NoError(t, err)
	exportReceipt(t, svc, st, Receipt{Query: "synthetic old version", Feedback: &Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: node.ID, ContentVersionID: old}}}})
	page, err = svc.Export(t.Context(), "cards", 1, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Skipped["identity_changed"])
}

func TestExportOversizedCardAdvancesPastRecord(t *testing.T) {
	svc, st := feedbackService(t)
	node, err := st.CreateFile(t.Context(), st.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	receipt := Receipt{Query: "synthetic", Feedback: &Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: node.ID}}}}
	for i := range 10 {
		hit := Hit{Rank: i + 1, NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		for j := range 64 {
			hit.EvidenceIDs = append(hit.EvidenceIDs, fmt.Sprintf("%d/%d", i, j)+strings.Repeat("\x00", 1010))
		}
		receipt.Hits = append(receipt.Hits, hit)
	}
	exportReceipt(t, svc, st, receipt)
	page, err := svc.Export(t.Context(), "cards", 100, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Skipped["item_too_large"])
	require.Equal(t, 1, page.NextOffset)
	require.True(t, page.Done)
}

func TestExportExactPageSizeCardAdvancesPastRecord(t *testing.T) {
	svc, st := feedbackService(t)
	node, err := st.CreateFile(t.Context(), st.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	receipt := Receipt{Feedback: &Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: node.ID}}}}
	item := ExportItem{SourceRecordID: strings.Repeat("a", 55), Expected: []Identity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}}, EvidenceIDs: []string{}}
	for i, count := range []int{64, 64, 43} {
		hit := Hit{Rank: i + 1, NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		for j := range count {
			id := fmt.Sprintf("%d/%d", i, j) + strings.Repeat("\x00", 1010)
			hit.EvidenceIDs = append(hit.EvidenceIDs, id)
			item.EvidenceIDs = append(item.EvidenceIDs, id)
		}
		receipt.Hits = append(receipt.Hits, hit)
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	padding := maxExportPageBytes - len(raw)
	require.Positive(t, padding)
	require.LessOrEqual(t, padding, 32768)
	receipt.Query = strings.Repeat("q", padding)
	item.Question = receipt.Query
	raw, err = json.Marshal(item)
	require.NoError(t, err)
	require.Len(t, raw, maxExportPageBytes)
	exportReceipt(t, svc, st, receipt)
	page, err := svc.Export(t.Context(), "cards", 100, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Skipped["item_too_large"])
	require.Equal(t, 1, page.NextOffset)
	require.True(t, page.Done)
}

func TestExportCSVPreservesFormulaAndCRLFBytes(t *testing.T) {
	question := "=SUM(1,2)\r\n\"synthetic\"\rbare"
	var output bytes.Buffer
	require.NoError(t, WriteEvalCSV(&output, []ExportItem{{SourceRecordID: "synthetic", Question: question, Expected: []Identity{}, EvidenceIDs: []string{}}}))
	require.True(t, strings.HasPrefix(output.String(), "id,question,expected_ids,scope\r\n"))
	require.Contains(t, output.String(), "\"=SUM(1,2)\r\n\"\"synthetic\"\"\rbare\"")
	require.True(t, strings.HasSuffix(output.String(), "\r\n"))
	var cards bytes.Buffer
	require.NoError(t, WriteCardsJSONL(&cards, []ExportItem{{SourceRecordID: "synthetic", Question: question, Expected: []Identity{}, EvidenceIDs: []string{}}}))
	var card map[string]any
	require.NoError(t, json.Unmarshal(cards.Bytes(), &card))
	require.Equal(t, question, card["question"])
	require.Equal(t, []any{}, card["expected"])
	require.Equal(t, []any{}, card["evidence_ids"])
	require.Len(t, card, 4)
	require.Error(t, WriteEvalCSV(failingExportWriter{}, []ExportItem{}))
	require.Error(t, WriteCardsJSONL(failingExportWriter{}, []ExportItem{{Question: "synthetic"}}))
}

type failingExportWriter struct{}

func (failingExportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func FuzzEvalCSVRoundTrip(f *testing.F) {
	for _, question := range []string{"synthetic", "=1+1", "comma,quote\"newline\n", "line\r\nnext", "é\x00"} {
		f.Add(question)
	}
	f.Fuzz(func(t *testing.T, question string) {
		if !utf8.ValidString(question) || len(question) > 32768 {
			return
		}
		var output bytes.Buffer
		require.NoError(t, WriteEvalCSV(&output, []ExportItem{{SourceRecordID: "id", Question: question, Expected: []Identity{{NodeID: 7}}, Scope: `{"fence":"synthetic"}`}}))
		raw := output.String()
		rows, err := csv.NewReader(strings.NewReader(raw)).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.Equal(t, []string{"id", "question", "expected_ids", "scope"}, rows[0])
		require.Equal(t, strings.ReplaceAll(question, "\r\n", "\n"), rows[1][1])
		require.Equal(t, "node:7", rows[1][2])
		if strings.ContainsAny(question, ",\"\r\n") {
			require.Contains(t, raw, "\""+strings.ReplaceAll(question, "\"", "\"\"")+"\"")
		}
	})
}

func FuzzCardsJSONLRoundTrip(f *testing.F) {
	f.Add("synthetic\r\nquestion", "evidence:synthetic")
	f.Add("=1+1", "quote\"newline\n")
	f.Fuzz(func(t *testing.T, question, evidence string) {
		if !utf8.ValidString(question) || !utf8.ValidString(evidence) || len(question) > 32768 || len(evidence) > 1024 {
			return
		}
		var output bytes.Buffer
		require.NoError(t, WriteCardsJSONL(&output, []ExportItem{{SourceRecordID: "synthetic", Question: question, Expected: []Identity{{NodeID: 7}}, EvidenceIDs: []string{evidence}, Scope: "private scope excluded"}}))
		var card map[string]any
		require.NoError(t, json.Unmarshal(output.Bytes(), &card))
		require.Len(t, card, 4)
		require.Equal(t, question, card["question"])
		require.Equal(t, []any{evidence}, card["evidence_ids"])
		require.Equal(t, "synthetic", card["source_record_id"])
	})
}
