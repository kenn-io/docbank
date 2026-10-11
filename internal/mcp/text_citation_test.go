package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func newCitationMCPFixture(t *testing.T) (*Server, document.TextCitation) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "spool"), 0o700))
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	hash, size, err := blobs.Write(strings.NewReader("MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\naé界🙂z\r\n"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(),
		"quote.eml", hash, size, "message/rfc822")
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	_, err = processing.EnsureEmailTarget(t.Context(), catalog, blobs, filepath.Join(root, "spool"),
		store.EmailTarget{Version: version})
	require.NoError(t, err)
	profile, err := processing.EmailBodyProfileFingerprint()
	require.NoError(t, err)
	view, err := catalog.ActiveRendition(t.Context(), version.ID, profile)
	require.NoError(t, err)
	reader, _, err := blobs.OpenStreamContext(t.Context(), view.Build.MarkdownChecksum)
	require.NoError(t, err)
	markdown, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	prefix, _, found := strings.Cut(string(markdown), "é界🙂")
	require.True(t, found)
	vaultID, err := uuid.Parse(catalog.VaultID())
	require.NoError(t, err)
	versionID, err := uuid.Parse(version.ID)
	require.NoError(t, err)
	start := utf8.RuneCountInString(prefix)
	citation := document.TextCitation{Version: 1, VaultUID: vaultID, NodeID: node.ID,
		ContentVersionID: versionID, ContentSHA256: hash,
		RenditionAttachmentID: view.Attachment.ID,
		BuildID:               view.Build.ID, RenditionSHA256: view.Build.MarkdownChecksum, Start: start, End: start + 3}
	gate := api.NewOperationGate()
	service, err := processing.NewService(processing.ServiceConfig{
		Catalog: catalog, Blobs: blobs, Gate: gate, SpoolDirectory: filepath.Join(root, "spool")})
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg,
		Gate: gate, Processing: service})
	t.Cleanup(daemon.Close)
	transport := httptest.NewServer(daemon.Handler())
	t.Cleanup(transport.Close)
	return newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
		exportTestLease(t, transport.URL)), citation
}

func TestMCPTextCitation(t *testing.T) {
	server, citation := newCitationMCPFixture(t)
	for _, transport := range []string{"stdio", "http"} {
		listed := decodeResult(t, exportExchange(t, server, transport, "tools/list", nil))
		tool := listedToolsByName(t, listed)["resolve_text_citation"]
		require.NotNil(t, tool)
		hints := objectField(t, tool, "annotations")
		require.Equal(t, true, hints["readOnlyHint"])
		require.Equal(t, true, hints["idempotentHint"])
		require.Equal(t, false, hints["destructiveHint"])
		result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
			"name": "resolve_text_citation", "arguments": citation,
		}))
		output := objectField(t, result, "structuredContent")
		require.Equal(t, "é界🙂", output["text"])
		require.EqualValues(t, 9, output["text_bytes"])
		require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("é界🙂"))), output["text_sha256"])
		encoded, err := json.Marshal(output["citation"])
		require.NoError(t, err)
		var got document.TextCitation
		require.NoError(t, json.Unmarshal(encoded, &got))
		require.Equal(t, citation, got)
		require.Equal(t, "private", output["cacheScope"])
		require.EqualValues(t, 0, output["ttlMs"])
	}
}

func citationMCPArguments() map[string]any {
	return map[string]any{"version": 1, "vault_uid": testVaultID, "node_id": 7,
		"content_version_id": testVersionID, "content_sha256": strings.Repeat("a", 64),
		"rendition_attachment_id": testAttachmentID, "build_id": testBuildID,
		"rendition_sha256": strings.Repeat("d", 64), "start": 0, "end": 3}
}

func TestMCPTextCitationFailures(t *testing.T) {
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errors.New("unexpected acquisition")
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	valid := citationMCPArguments()
	var invalid []map[string]any
	for name := range valid {
		missing := maps.Clone(valid)
		delete(missing, name)
		invalid = append(invalid, missing)
		null := maps.Clone(valid)
		null[name] = nil
		invalid = append(invalid, null)
	}
	for _, change := range []map[string]any{
		{"extra": true}, {"version": 2}, {"start": 3}, {"end": 16001}, {"start": -1},
		{"end": 2147483648}, {"node_id": 0}, {"content_sha256": strings.Repeat("A", 64)},
	} {
		args := maps.Clone(valid)
		maps.Copy(args, change)
		invalid = append(invalid, args)
	}
	for _, field := range []string{"vault_uid", "content_version_id"} {
		for _, value := range []string{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
			"urn:uuid:" + testVaultID, "{" + testVaultID + "}", strings.ReplaceAll(testVaultID, "-", ""),
			"11111111-1111-1111-8111-111111111111"} {
			args := maps.Clone(valid)
			args[field] = value
			invalid = append(invalid, args)
		}
	}
	for _, args := range invalid {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "resolve_text_citation", "arguments": args,
		}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code, args)
	}
	require.Zero(t, acquisitions.Load())
}
