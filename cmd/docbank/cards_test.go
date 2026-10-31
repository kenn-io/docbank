package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"

	"github.com/stretchr/testify/require"
)

func TestCardsCheckListsFiles(t *testing.T) {
	setupVaultHome(t)
	file := writeSourceFile(t, "notes.txt", "ordinary synthetic text")
	_, err := runCLI(t, "add", file, "--dest", "/answer-cards")
	require.NoError(t, err)
	out, err := runCLI(t, "cards", "check", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"state":"not_a_card"`)
	require.Contains(t, out, "notes.txt")
}

func TestCardsCheckReportsSourceChange(t *testing.T) {
	setupVaultHome(t)
	source := writeSourceFile(t, "source.txt", "test")
	_, err := runCLI(t, "add", source, "--dest", "/sources")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(context.Background())
	require.NoError(t, err)
	info, err := c.API().VaultInfo(context.Background())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(context.Background(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/sources/source.txt"}})
	require.NoError(t, err)
	digest := strings.Repeat("a", 64)
	content := fmt.Sprintf(`---
docbank_answer_card: 1
question: What is the source text?
citations:
  - version: 1
    vault_uid: %s
    node_id: %d
    content_version_id: %s
    content_sha256: %s
    rendition_attachment_id: %s
    build_id: %s
    rendition_sha256: %s
    start: 0
    end: 4
    quote: test
state: draft
---
Test answer.
`, info.VaultID, node.ID, node.CurrentVersionID, node.BlobHash, digest, digest, digest)
	card, err := answercard.Parse([]byte(content))
	require.NoError(t, err)
	for _, name := range []string{"card.md", "card.txt"} {
		file := writeSourceFile(t, name, content)
		_, err = runCLI(t, "add", file, "--dest", "/answer-cards")
		require.NoError(t, err)
	}
	imported, err := c.API().ResolvePath(context.Background(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/answer-cards/card.txt"}})
	require.NoError(t, err)
	require.Equal(t, "text/plain; charset=utf-8", imported.MimeType)
	foreign := writeSourceFile(t, "foreign.md", strings.Replace(content, info.VaultID, "00000000-0000-4000-8000-000000000099", 1))
	_, err = runCLI(t, "add", foreign, "--dest", "/answer-cards")
	require.NoError(t, err)
	out, err := runCLI(t, "cards", "check", "--deep", "--json")
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(out, `"state":"draft"`))
	require.Contains(t, out, "card.md")
	require.Contains(t, out, "card.txt")
	require.Contains(t, out, `"state":"invalid"`)
	require.Contains(t, out, "citation belongs to another vault")
	require.Contains(t, out, `"reason":"quote_unavailable"`)
	content = strings.Replace(content, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
	approved := writeSourceFile(t, "card.md", content)
	_, err = runCLI(t, "put", approved, "/answer-cards/card.md")
	require.NoError(t, err)
	out, err = runCLI(t, "cards", "check", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"state":"approved"`)
	replacement := writeSourceFile(t, "new.txt", "changed")
	_, err = runCLI(t, "put", replacement, "/sources/source.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "cards", "check", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"reason":"new_version"`)
}

func TestCardsCheckInvalidCardHasSingleRow(t *testing.T) {
	setupVaultHome(t)
	file := writeSourceFile(t, "broken.md", "---\ndocbank_answer_card: 1\nunknown_field: value\n---\nAnswer.\n")
	_, err := runCLI(t, "add", file, "--dest", "/answer-cards")
	require.NoError(t, err)
	file = writeSourceFile(t, "future.md", "---\ndocbank_answer_card: 2\n---\nAnswer.\n")
	_, err = runCLI(t, "add", file, "--dest", "/answer-cards")
	require.NoError(t, err)
	out, err := runCLI(t, "cards", "check")
	require.NoError(t, err)
	rows := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, rows, 3)
	require.Contains(t, rows[1], "broken.md")
	require.Contains(t, rows[1], "invalid")
	require.Contains(t, rows[2], "future.md")
	require.Contains(t, rows[2], "invalid")
}
