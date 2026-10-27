package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	for _, tc := range []struct {
		body      string
		wantError string
	}{
		{`{"unknown":true}`, "must contain one exact text citation"},
		{`{} {}`, "must contain one exact text citation"},
		{strings.Repeat(" ", 16<<10+1), "exceeds 16 KiB"},
	} {
		require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
		_, err := readKataCitation(path)
		require.ErrorContains(t, err, tc.wantError)
	}
}

func TestKataContextPrintsPassagesAndPropagatesWriteErrors(t *testing.T) {
	result := kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Docbank: &kataevidence.DocumentReference{NodeID: 2}}}, SavedQuote: "hello"},
		{State: "unsupported"},
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Rendition: &document.TextCitation{NodeID: 3, Start: 1, End: 3, ContentVersionID: uuid.MustParse("22222222-2222-4222-8222-222222222222")}}}},
	}, NextOffset: 10}
	pendingEvidence := kataevidence.NewEvidence(kataevidence.Reference{Version: 1, Kind: "docbank", Docbank: &kataevidence.DocumentReference{
		VaultUID: "11111111-1111-4111-8111-111111111111", NodeID: 2,
		ContentVersionID: "22222222-2222-4222-8222-222222222222", ContentSHA256: strings.Repeat("a", 64), EndRune: 5,
	}}, "hello", "notes.txt")
	for _, tc := range []struct {
		name   string
		result kataevidence.Context
		want   []string
	}{
		{"passages", result, []string{"available id:2", "hello\nunsupported\n", "Continue with --offset 10\n", "available id:3 [1,3) version 22222222-2222-4222-8222-222222222222\n"}},
		{"pending_comment", kataevidence.Context{Passages: []kataevidence.Resolution{{State: "available", Evidence: pendingEvidence, Pending: &kataevidence.Pending{SavedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, SavedQuote: "hello"}}}, []string{"comment pending", "hello"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writes := 0
			require.NoError(t, writeKataContext(reportTestWriter(func(p []byte) (int, error) {
				writes++
				return out.Write(p)
			}), tc.result))
			for _, want := range tc.want {
				require.Contains(t, out.String(), want)
			}
			for failAt := 1; failAt <= writes; failAt++ {
				attempt := 0
				err := writeKataContext(reportTestWriter(func(p []byte) (int, error) {
					attempt++
					if attempt == failAt {
						return 0, io.ErrClosedPipe
					}
					return len(p), nil
				}), tc.result)
				require.ErrorIs(t, err, io.ErrClosedPipe, "write %d: %v", failAt, err)
			}
		})
	}
}
