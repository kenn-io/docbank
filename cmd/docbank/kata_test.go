package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataCiteValidatesBeforeDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "kata", "cite", "id:42", "--title", "Follow up", "--idempotency-key", "follow-up")
	require.ErrorContains(t, err, "range requires --end")
}

func TestKataCiteCitationSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "citation.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"vault_uid":"11111111-1111-4111-8111-111111111111","node_id":42,"content_version_id":"22222222-2222-4222-8222-222222222222","content_sha256":"`+strings.Repeat("d", 64)+`","rendition_attachment_id":"`+strings.Repeat("a", 64)+`","build_id":"`+strings.Repeat("b", 64)+`","rendition_sha256":"`+strings.Repeat("c", 64)+`","start":6,"end":11}`), 0o600))
	command, _, err := newKataCommand().Find([]string{"cite"})
	require.NoError(t, err)
	require.NoError(t, command.ParseFlags([]string{"--citation", path, "--issue", "docbank#abcd"}))
	selection, err := kataCiteSelector(command, "id:42", kataevidence.Selector{Quote: "hello"}, path)
	require.NoError(t, err)
	require.EqualValues(t, 42, selection.NodeID)
	require.Equal(t, 6, selection.StartRune)
	require.Equal(t, 11, selection.EndRune)
	require.Equal(t, "22222222-2222-4222-8222-222222222222", selection.ExpectedVersion)
	require.Equal(t, "hello", selection.Quote)
	require.NotNil(t, selection.Citation)
	require.Equal(t, strings.Repeat("a", 64), selection.Citation.RenditionAttachmentID)
	selection, err = kataCiteSelector(command, "/notes.txt", kataevidence.Selector{ExpectedVersion: "33333333-3333-4333-8333-333333333333"}, path)
	require.NoError(t, err)
	require.Equal(t, "/notes.txt", selection.Path)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", selection.ExpectedVersion)
	for _, flag := range []string{"--start", "--end"} {
		command, _, err := newKataCommand().Find([]string{"cite"})
		require.NoError(t, err)
		require.NoError(t, command.ParseFlags([]string{"--citation", path, flag, "0"}))
		_, err = kataCiteSelector(command, "id:42", kataevidence.Selector{}, path)
		require.ErrorContains(t, err, "--citation supplies the range")
	}
}

func TestKataCitationRejectsMalformedAndOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "citation.json")
	for _, body := range []string{`{"unknown":true}`, `{} {}`, strings.Repeat(" ", 16<<10+1)} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := readKataCitation(path)
		if len(body) > 16<<10 {
			require.ErrorContains(t, err, "exceeds 16 KiB")
		} else {
			require.ErrorContains(t, err, "must contain one exact text citation")
		}
	}
}

func TestKataContextPrintsPassagesAndPropagatesWriteErrors(t *testing.T) {
	result := kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Docbank: &kataevidence.DocumentReference{NodeID: 2}}}, SavedQuote: "hello"},
		{State: "unsupported"},
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Rendition: &document.TextCitation{NodeID: 3, Start: 1, End: 3, ContentVersionID: uuid.MustParse("22222222-2222-4222-8222-222222222222")}}}},
	}, NextOffset: 10}
	var out bytes.Buffer
	require.NoError(t, writeKataContext(&out, result))
	require.Contains(t, out.String(), "available id:2")
	require.Contains(t, out.String(), "hello\nunsupported\n")
	require.Contains(t, out.String(), "Continue with --offset 10\n")
	require.Contains(t, out.String(), "available id:3 [1,3) version 22222222-2222-4222-8222-222222222222\n")
	for failAt := 1; failAt <= 6; failAt++ {
		writes := 0
		err := writeKataContext(reportTestWriter(func(p []byte) (int, error) {
			writes++
			if writes == failAt {
				return 0, io.ErrClosedPipe
			}
			return len(p), nil
		}), result)
		require.ErrorIs(t, err, io.ErrClosedPipe, "write %d: %v", failAt, err)
	}
}

func TestKataContextPrintsPendingComment(t *testing.T) {
	var result kataevidence.Context
	require.NoError(t, json.Unmarshal([]byte(`{"passages":[{"state":"available","evidence":{},"pending":{"saved_at":"2026-01-01T00:00:00Z"},"saved_quote":"hello"}]}`), &result))
	var out bytes.Buffer
	require.NoError(t, writeKataContext(&out, result))
	require.Contains(t, out.String(), "comment pending")
	require.Contains(t, out.String(), "hello")
}

func TestKataCiteHelpExplainsBinaryCitation(t *testing.T) {
	command, _, err := newKataCommand().Find([]string{"cite"})
	require.NoError(t, err)
	var out bytes.Buffer
	command.SetOut(&out)
	require.NoError(t, command.Help())
	require.Contains(t, out.String(), "read_rendition_text")
	require.Contains(t, out.String(), "list_document_versions")
}
