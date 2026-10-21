package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataCiteValidatesBeforeDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"kata", "cite", "id:42", "--title", "Follow up", "--idempotency-key", "follow-up"}, "range requires --end"},
		{[]string{"kata", "cite", "id:42", "--citation", "unused.json", "--start", "0"}, "--citation supplies the range"},
		{[]string{"kata", "cite", "id:42", "--citation", "unused.json", "--end", "5"}, "--citation supplies the range"},
	} {
		_, err := runCLI(t, tc.args...)
		require.ErrorContains(t, err, tc.message)
	}
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
}

func TestKataCitationRejectsMalformedAndOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "citation.json")
	for _, body := range []string{`{"unknown":true}`, `{} {}`, strings.Repeat(" ", 16<<10+1)} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := readKataCitation(path)
		require.Error(t, err)
	}
}

func TestKataCLIRequiresRange(t *testing.T) {
	_, err := runCLI(t, "kata", "cite", "id:2", "--title", "Follow up", "--idempotency-key", "test")
	require.ErrorContains(t, err, "range requires --end")
}

func TestKataContextPrintsEveryStatus(t *testing.T) {
	var out bytes.Buffer
	err := writeKataContext(&out, kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "unsupported"},
		{State: "failed", SavedQuote: "Saved words"},
	}})
	require.NoError(t, err)
	require.Contains(t, out.String(), "unsupported\nfailed\nSaved words\n")
}

type kataFailWriter struct {
	writes int
	failAt int
}

func (w *kataFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestKataContextPropagatesOutputErrors(t *testing.T) {
	result := kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Docbank: &kataevidence.DocumentReference{NodeID: 2}}}, SavedQuote: "hello"},
		{State: "unsupported"},
	}, NextOffset: 10}
	for failAt := 1; failAt <= 5; failAt++ {
		err := writeKataContext(&kataFailWriter{failAt: failAt}, result)
		require.ErrorIs(t, err, io.ErrClosedPipe, "write %d: %v", failAt, err)
	}
}
