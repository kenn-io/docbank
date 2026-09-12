package client_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/client"
)

// This opt-in proof owns a synthetic vault and a real compiled daemon. It does
// not install relation rows, substitute decoder results, or mock browser APIs.
func TestAttachmentNavigationRealDaemonBrowser(t *testing.T) {
	screenshots := os.Getenv("DOCBANK_ATTACHMENT_NAVIGATION_SCREENSHOT_DIR")
	if screenshots == "" {
		t.Skip("requires the built binary and opt-in synthetic browser screenshots")
	}
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	vault := filepath.Join(t.TempDir(), "vault")
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), filepath.Join(repository, "docbank"), args...)
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+vault)
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, string(out))
		return out
	}
	run("daemon", "start")
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(repository, "docbank"), "daemon", "stop")
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+vault)
		out, stopErr := cmd.CombinedOutput()
		require.NoError(t, stopErr, string(out))
	})
	record, _, found, err := client.Find(t.Context(), vault)
	require.NoError(t, err)
	require.True(t, found)
	c := client.New("http://"+record.Address, record.Metadata["api_key"])
	hash := func(raw string) string { sum := sha256.Sum256([]byte(raw)); return hex.EncodeToString(sum[:]) }
	root, err := c.Stat(t.Context(), "/")
	require.NoError(t, err)
	nested := "Subject: Synthetic nested message\r\nContent-Type: multipart/mixed; boundary=n\r\n\r\n--n\r\nContent-Type: text/plain\r\n\r\nNested body\r\n--n\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=inside.txt\r\n\r\nExact nested attachment bytes.\r\n--n--\r\n"
	var raw strings.Builder
	raw.WriteString("Subject: Synthetic attachment family\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nSynthetic family body\r\n")
	for range 51 {
		raw.WriteString("--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=repeated.txt\r\n\r\nPinned child edition, verified from its exact version.\r\n")
	}
	raw.WriteString("--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=nested.eml\r\n\r\n" + nested + "\r\n--m--\r\n")
	upload, err := c.Upload(t.Context(), root.ID, "01-family.eml", "message/rfc822", hash(raw.String()), int64(raw.Len()), strings.NewReader(raw.String()))
	require.NoError(t, err)
	publish := func(versionID, operation string) document.EmailDocumentPublicationReceipt {
		t.Helper()
		view, readErr := c.EnsureEmailMetadata(t.Context(), versionID)
		require.NoError(t, readErr)
		dir, readErr := c.Node(t.Context(), root.ID)
		require.NoError(t, readErr)
		receipt, publishErr := c.PublishEmailDocuments(t.Context(), document.EmailDocumentPublicationRequest{
			OperationID: operation, Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID, SHA256: view.Version.BlobHash, Size: view.Version.Size},
			GenerationID: view.GenerationID, AttachmentID: view.AttachmentID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{},
		})
		require.NoError(t, publishErr)
		return receipt
	}
	receipt := publish(upload.Node.CurrentVersionID, "synthetic-family")
	require.Len(t, receipt.Relations, 52)
	require.Equal(t, receipt.Relations[0].Child.SHA256, receipt.Relations[50].Child.SHA256)
	require.NotEqual(t, receipt.Relations[0].Child.NodeID, receipt.Relations[50].Child.NodeID)
	nestedReceipt := publish(receipt.Relations[51].Child.VersionID, "synthetic-nested")
	require.Len(t, nestedReceipt.Relations, 1)
	child, err := c.Node(t.Context(), receipt.Relations[0].Child.NodeID)
	require.NoError(t, err)
	const replacement = "New live head must never replace the related historical edition."
	_, err = c.ReplaceContent(t.Context(), child.ID, child.Revision, "application/octet-stream", hash(replacement), int64(len(replacement)), strings.NewReader(replacement))
	require.NoError(t, err)
	launch, err := c.WebSessionURL(t.Context())
	require.NoError(t, err)
	fixture, err := json.Marshal(struct {
		Parent document.EmailDocumentIdentity `json:"parent"`
		Child  document.EmailDocumentIdentity `json:"child"`
		Nested document.EmailDocumentIdentity `json:"nested"`
		Inside document.EmailDocumentIdentity `json:"inside"`
	}{receipt.Relations[0].Parent, *receipt.Relations[0].Child, *receipt.Relations[51].Child, *nestedReceipt.Relations[0].Child})
	require.NoError(t, err)
	playwright := exec.CommandContext(t.Context(), "node", filepath.Join(repository, "frontend/node_modules/@playwright/test/cli.js"), "test", "attachment-navigation.screenshot.ts", "--config", filepath.Join(repository, "frontend/screenshots/playwright.config.ts"), "--project", "chromium", "--grep", "DB24 actual attachment navigation")
	playwright.Dir = repository
	playwright.Env = append(os.Environ(), "DOCBANK_DB24_BROWSER_URL="+launch, "DOCBANK_DB24_FIXTURE="+string(fixture))
	out, err := playwright.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Log(string(out))
}
