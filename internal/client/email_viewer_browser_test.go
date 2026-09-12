package client_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/client"
)

// MAIL07 uses only a synthetic vault, real decoder and compiled daemon. The
// attacker listener independently witnesses requests before and after mount.
func TestEmailViewerRealDaemonBrowser(t *testing.T) {
	if os.Getenv("DOCBANK_EMAIL_VIEWER_SCREENSHOT_DIR") == "" {
		t.Skip("opt-in real daemon MAIL07 browser proof")
	}
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	vault := filepath.Join(t.TempDir(), "vault")
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), filepath.Join(repository, "docbank"), args...)
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+vault)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return out
	}
	run("daemon", "start")
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(repository, "docbank"), "daemon", "stop")
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+vault)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		status := exec.Command(filepath.Join(repository, "docbank"), "daemon", "status", "--json")
		status.Env = append(os.Environ(), "DOCBANK_HOME="+vault)
		out, err = status.CombinedOutput()
		require.NoError(t, err, string(out))
		var stopped struct {
			Running bool `json:"running"`
		}
		require.NoError(t, json.Unmarshal(out, &stopped))
		require.False(t, stopped.Running)
		t.Log("MAIL07 cleanup: synthetic daemon stopped; temporary vault is owned by testing.TempDir")
	})
	record, _, found, err := client.Find(t.Context(), vault)
	require.NoError(t, err)
	require.True(t, found)
	c := client.New("http://"+record.Address, record.Metadata["api_key"])
	var trackingRequests atomic.Int64
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		trackingRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tracker.Close()
	root, err := c.Stat(t.Context(), "/")
	require.NoError(t, err)
	hash := func(raw string) string { sum := sha256.Sum256([]byte(raw)); return hex.EncodeToString(sum[:]) }
	add := func(name, raw string) document.EmailDocumentIdentity {
		t.Helper()
		upload, err := c.Upload(t.Context(), root.ID, name, "message/rfc822", hash(raw), int64(len(raw)), strings.NewReader(raw))
		require.NoError(t, err)
		_, err = c.EnsureEmailMetadata(t.Context(), upload.Node.CurrentVersionID)
		require.NoError(t, err)
		return document.EmailDocumentIdentity{NodeID: upload.Node.ID, VersionID: upload.Node.CurrentVersionID, SHA256: hash(raw), Size: int64(len(raw))}
	}
	picture := image.NewRGBA(image.Rect(0, 0, 160, 48))
	for y := range 48 {
		for x := range 160 {
			picture.Set(x, y, color.RGBA{R: 38, G: 109, B: 162, A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, picture))
	payload := base64.StdEncoding.EncodeToString(encoded.Bytes())
	imagePart := func(cid string) string {
		return "Content-Type: image/png\r\nContent-ID: <" + cid + ">\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: base64\r\n\r\n" + payload + "\r\n"
	}
	attack := tracker.URL
	html := `<h2>Project Atlas briefing</h2><p>A synthetic archived message with a verified local illustration.</p><img src="cid:logo" alt="Atlas blue banner"><img src="cid:duplicate" alt="Ambiguous resource"><img src="cid:missing" alt="Missing resource"><img src="` + attack + `/pixel" alt="Blocked tracker"><img srcset="` + attack + `/srcset 2x"><picture><source srcset="` + attack + `/source"><img src="` + attack + `/fallback"></picture><link rel="stylesheet" href="` + attack + `/css"><link rel="preload" as="image" href="` + attack + `/preload"><style>@import '` + attack + `/import'; p { background:url(` + attack + `/background) }</style><p style="background-image:url(` + attack + `/inline)" onclick="window.senderExecuted=true">Quarterly notes</p><script>window.senderExecuted=true;fetch('` + attack + `/script')</script><iframe src="` + attack + `/frame"></iframe><svg><image href="` + attack + `/svg"/></svg><form action="` + attack + `/form"><input autofocus onfocus="fetch('` + attack + `/focus')"><button>Submit</button></form><object data="` + attack + `/object"></object><meta http-equiv="refresh" content="0;url=` + attack + `/refresh"><a href="` + attack + `/link">Blocked outbound link</a><blockquote>` + strings.Repeat("Complete quoted evidence stays available for PDF export. ", 20) + `</blockquote>` + strings.Repeat("<p>Long-mail reading line: retained synthetic project history.</p>", 45) + `<p>End of complete HTML body.</p>`
	raw := "Subject: =?utf-8?Q?Project_Atlas_=E2=80=94_briefing?=\r\nFrom: Archive Team <archive@example.test>\r\nTo: Reader <reader@example.test>\r\nX-Trace: first\r\nX-Trace: second\r\nDate: Fri, 11 Sep 2026 09:30:00 +0530\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlain alternative remains complete.\r\n--a\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + html + "\r\n--a--\r\n--r\r\n" + imagePart("logo") + "--r\r\n" + imagePart("duplicate") + "--r\r\n" + imagePart("duplicate") + "--r\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=notes.txt\r\n\r\nSynthetic attachment exact bytes.\r\n--r--\r\n"
	htmlID := add("01-atlas.eml", raw)
	view, err := c.EmailMetadata(t.Context(), htmlID.VersionID)
	require.NoError(t, err)
	dir, err := c.Node(t.Context(), root.ID)
	require.NoError(t, err)
	receipt, err := c.PublishEmailDocuments(t.Context(), document.EmailDocumentPublicationRequest{OperationID: "synthetic-viewer-family", Parent: htmlID, GenerationID: view.GenerationID, AttachmentID: view.AttachmentID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{}})
	require.NoError(t, err)
	plainID := add("02-plain.eml", "Subject: Plain text field notes\r\nFrom: Notes <notes@example.test>\r\nDate: invalid sender date\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlain text keeps <script> inert.\r\n"+strings.Repeat("Long plain-text archive line.\r\n", 120)+"End of complete plain body.")
	partialID := add("03-partial.eml", "Subject: Partial attachment inventory\r\nContent-Type: multipart/mixed; boundary=p\r\n\r\n--p\r\nContent-Type: text/plain\r\n\r\nThe readable body survives an incomplete MIME inventory.\r\n--p\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=broken.txt\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!bad base64!!!\r\n--p\r\nX: unfinished headers\r\n")
	partial, err := c.EmailMetadata(t.Context(), partialID.VersionID)
	require.NoError(t, err)
	require.Equal(t, document.EmailInventoryPartial, partial.Evidence.Inventory.State)
	launch, err := c.WebSessionURL(t.Context())
	require.NoError(t, err)
	fixture, err := json.Marshal(map[string]any{"html": htmlID, "plain": plainID, "partial": partialID, "tracker": attack, "relations": receipt.Relations})
	require.NoError(t, err)
	playwright := exec.CommandContext(t.Context(), "node", filepath.Join(repository, "frontend/node_modules/@playwright/test/cli.js"), "test", "email-viewer.screenshot.ts", "--config", filepath.Join(repository, "frontend/screenshots/playwright.config.ts"), "--project", "chromium")
	playwright.Dir = repository
	playwright.Env = append(os.Environ(), "DOCBANK_DB28_BROWSER_URL="+launch, "DOCBANK_DB28_FIXTURE="+string(fixture))
	out, err := playwright.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Log(string(out))
	require.Zero(t, trackingRequests.Load(), "sender listener observed an unauthorized request")
	t.Log("MAIL07 independent attacker listener: zero requests; synthetic daemon cleanup registered")
}
