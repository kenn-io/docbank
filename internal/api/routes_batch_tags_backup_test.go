package api_test

import (
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestBatchTagReceiptSurvivesPhysicalBackupRestore(t *testing.T) {
	for _, driver := range collectionBackupDrivers() {
		t.Run(driver.Name(), func(t *testing.T) {
			ts, live := newCollectionBackupServer(t, driver, filepath.Join(t.TempDir(), "repository"))
			node, err := live.Mkdir(t.Context(), live.RootID(), "selected")
			require.NoError(t, err)
			tag, err := live.CreateTag(t.Context(), "Review")
			require.NoError(t, err)
			c := client.New(ts.URL, testAPIKey)
			preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{NodeID: live.RootID(), AgentLabel: "batch-tag-backup-test"})
			require.NoError(t, err)
			_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
			require.NoError(t, err)
			node, err = live.NodeByID(t.Context(), node.ID)
			require.NoError(t, err)
			request := store.BatchTagRequest{OperationID: "11111111-1111-4111-8111-111111111111", TagID: tag.ID, Assign: true,
				Nodes: []store.BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}}}
			original, err := live.BatchTags(t.Context(), request)
			require.NoError(t, err)
			resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{"tag": "batch-tag-receipt", "jobs": 1})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var snapshot api.BackupSnapshot
			require.NoError(t, json.Unmarshal([]byte(body), &snapshot))
			target := filepath.Join(t.TempDir(), "restored")
			resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore/stream", nil,
				map[string]any{"target": target, "snapshot_id": snapshot.ID, "jobs": 1})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			events := decodeBackupRestoreEvents(t, body)
			require.NotEmpty(t, events)
			terminal := events[len(events)-1]
			require.Equal(t, "result", terminal.Type)
			require.NotNil(t, terminal.Report)
			require.True(t, terminal.Report.Proof.ContentVerified)
			restored, err := store.Open(filepath.Join(target, "docbank.db"), driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, restored.Close()) }()
			before, err := restored.AuditHistory(t.Context(), node.ID, 50, "")
			require.NoError(t, err)
			replay, err := restored.BatchTags(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, original, replay)
			after, err := restored.AuditHistory(t.Context(), node.ID, 50, "")
			require.NoError(t, err)
			require.Equal(t, before, after)
			kinds := make([]string, 0, len(after.Items))
			for _, event := range after.Items {
				kinds = append(kinds, event.Kind)
			}
			require.Contains(t, kinds, "tag_assign")
			_, err = restored.VerifyAudit(t.Context(), nil)
			require.NoError(t, err)
		})
	}
}
