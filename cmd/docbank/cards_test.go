package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"

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
	table, err := runCLI(t, "cards", "check", "--deep", "--json=false")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(table, "/answer-cards/card.md"))
	require.Equal(t, 1, strings.Count(table, "/answer-cards/card.txt"))
	require.Equal(t, 2, strings.Count(table, "new_version"))
	require.Contains(t, table, "citation 0: new_version")
	out, err = runCLI(t, "cards", "check", "--run", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"reason":"new_version"`)
	latched, err := c.API().ResolvePath(context.Background(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/answer-cards/card.md"}})
	require.NoError(t, err)
	_, err = runCLI(t, "cards", "check", "--run")
	require.NoError(t, err)
	again, err := c.API().ResolvePath(context.Background(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/answer-cards/card.md"}})
	require.NoError(t, err)
	require.Equal(t, latched.Revision, again.Revision)
	_, err = runCLI(t, "rm", "/sources/source.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "cards", "check", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"reason":"new_version"`)
}

// Seed synthetic retained evidence before the daemon acquires vault ownership.
// Approval itself and every assertion use the ordinary daemon API.
func setupApprovalCard(t *testing.T, quote string) (*daemonconn.Connection, api.Node) {
	t.Helper()
	root := t.TempDir()
	f := newReportRenditionFixture(t)
	catalog, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	receipt, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader("synthetic source"))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	written, err := catalog.CreateFileWithReceipt(t.Context(), catalog.RootID(), "source.pdf",
		receipt.Hash, receipt.Size, "application/pdf", store.BlobPhysical{
			Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible,
			MD5: receipt.MD5, Created: receipt.Created,
		})
	require.NoError(t, err)
	attachment := f.publish(t, catalog, blobs, written.Node, document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: "test",
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	})
	build, err := catalog.RenditionBuild(t.Context(), attachment.BuildID)
	require.NoError(t, err)
	reader, err := blobs.OpenContext(t.Context(), build.MarkdownChecksum)
	require.NoError(t, err)
	markdown, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	prefix, _, found := strings.Cut(string(markdown), "test")
	require.True(t, found)
	start := utf8.RuneCountInString(prefix)
	content := fmt.Sprintf(`---
docbank_answer_card: 1
question: What is the source text?
alternates:
  - source text
citations:
  - version: 1
    vault_uid: %s
    node_id: %d
    content_version_id: %s
    content_sha256: %s
    rendition_attachment_id: %s
    build_id: %s
    rendition_sha256: %s
    start: %d
    end: %d
    quote: %q
state: draft
---
Test answer.
`, catalog.VaultID(), written.Node.ID, written.Node.CurrentVersionID, written.Node.BlobHash,
		attachment.ID, build.ID, build.MarkdownChecksum, start, start+4, quote)
	require.NoError(t, blobs.Close())
	require.NoError(t, catalog.Close())
	fixture := pdfReportFixture{reportRenditionFixture: f, root: root}
	t.Cleanup(fixture.start(t, root))
	file := writeSourceFile(t, "card.md", content)
	_, err = runCLI(t, "add", file, "--dest", "/answer-cards")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	return c, resolveApprovalCard(t, c)
}

func resolveApprovalCard(t *testing.T, c *daemonconn.Connection) api.Node {
	t.Helper()
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/answer-cards/card.md"},
	})
	require.NoError(t, err)
	return *node
}

func readApprovalCard(t *testing.T, c *daemonconn.Connection, node api.Node) ([]byte, answercard.Card) {
	t.Helper()
	stream, err := c.VersionContent(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()
	var data bytes.Buffer
	_, err = stream.CopyVerified(&data)
	require.NoError(t, err)
	card, err := answercard.Parse(data.Bytes())
	require.NoError(t, err)
	return data.Bytes(), card
}

func TestCardsApproveRetainedQuote(t *testing.T) {
	for _, quote := range []string{"test", ""} {
		t.Run(fmt.Sprintf("quote=%q", quote), func(t *testing.T) {
			c, before := setupApprovalCard(t, quote)
			out, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
			require.NoError(t, err)
			require.Contains(t, out, "Question: What is the source text?")
			require.Contains(t, out, "Alternate: source text")
			require.Contains(t, out, "Answer: Test answer.")
			require.Contains(t, out, "[1] /source.pdf: test")
			after := resolveApprovalCard(t, c)
			require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
			_, card := readApprovalCard(t, c, after)
			require.Equal(t, "test", card.Citations[0].Quote)
			require.Equal(t, answercard.Approved, card.Effective(nil).State)
			require.Equal(t, card.Hash(), card.ApprovedHash)
			require.NotEmpty(t, card.ApprovedAt)
		})
	}
}

func TestCardsApproveRefusesQuoteMismatch(t *testing.T) {
	c, before := setupApprovalCard(t, "fake")
	_, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
	require.ErrorContains(t, err, "quote_mismatch")
	require.Equal(t, before.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
}

func TestCardsApproveRefusesConcurrentCardEdit(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	data, _ := readApprovalCard(t, c, before)
	var edited api.Node
	err := answercard.Approve(t.Context(), c, before, func(answercard.Card, []string) (bool, error) {
		replacement := writeSourceFile(t, "edited.md", strings.Replace(string(data), "Test answer.", "Edited answer.", 1))
		_, err := runCLI(t, "put", replacement, "/answer-cards/card.md")
		require.NoError(t, err)
		edited = resolveApprovalCard(t, c)
		return true, nil
	})
	require.ErrorContains(t, err, "stale_revision")
	c, err = daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	after := resolveApprovalCard(t, c)
	require.Equal(t, edited.CurrentVersionID, after.CurrentVersionID)
	_, card := readApprovalCard(t, c, after)
	require.Equal(t, "Edited answer.", card.Answer)
	require.Equal(t, answercard.Draft, card.Effective(nil).State)
}

func TestCardsApproveRefusesSourceChangedDuringConfirmation(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	err := answercard.Approve(t.Context(), c, before, func(answercard.Card, []string) (bool, error) {
		replacement := writeSourceFile(t, "changed.pdf", "changed synthetic source")
		_, err := runCLI(t, "put", replacement, "/source.pdf")
		require.NoError(t, err)
		return true, nil
	})
	require.ErrorContains(t, err, "citation_not_current")
	require.Equal(t, before.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
}

func TestCardsApproveRefusesUnavailableQuote(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	data, card := readApprovalCard(t, c, before)
	missing := strings.Replace(string(data), card.Citations[0].RenditionAttachmentID, strings.Repeat("f", 64), 1)
	file := writeSourceFile(t, "missing.md", missing)
	_, err := runCLI(t, "put", file, "/answer-cards/card.md")
	require.NoError(t, err)
	before = resolveApprovalCard(t, c)
	_, err = runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
	require.ErrorContains(t, err, "citation_unavailable")
	require.Equal(t, before.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
}

func TestCardsApproveCancelledEmptyQuoteWritesNothing(t *testing.T) {
	c, before := setupApprovalCard(t, "")
	displayed := false
	err := answercard.Approve(t.Context(), c, before, func(card answercard.Card, paths []string) (bool, error) {
		displayed = true
		require.Equal(t, "test", card.Citations[0].Quote)
		require.Equal(t, []string{"/source.pdf"}, paths)
		return false, nil
	})
	require.ErrorContains(t, err, "approval cancelled")
	require.True(t, displayed)
	require.Equal(t, before.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
}

func TestCardsCheckLatchedTrashRequiresReapproval(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	_, original := readApprovalCard(t, c, before)
	_, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/source.pdf")
	require.NoError(t, err)
	_, err = runCLI(t, "cards", "check", "--run")
	require.NoError(t, err)
	latched := resolveApprovalCard(t, c)
	_, card := readApprovalCard(t, c, latched)
	require.Equal(t, answercard.Stale, card.Effective(nil).State)
	require.Equal(t, "trashed", card.Stale.Reason)
	_, err = runCLI(t, "cards", "check", "--run")
	require.NoError(t, err)
	require.Equal(t, latched.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
	_, err = runCLI(t, "restore", formatNodeSelector(original.Citations[0].NodeID))
	require.NoError(t, err)
	out, err := runCLI(t, "cards", "check", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"state":"stale"`)
	require.Equal(t, latched.CurrentVersionID, resolveApprovalCard(t, c).CurrentVersionID)
	_, err = runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
	require.NoError(t, err)
	_, card = readApprovalCard(t, c, resolveApprovalCard(t, c))
	require.Nil(t, card.Stale)
	require.Equal(t, answercard.Approved, card.Effective(nil).State)
}

func TestCardsApproveFinalConfirmationLine(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	rootCmd.SetIn(strings.NewReader("y"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	_, err := runCLI(t, "cards", "approve", "/answer-cards/card.md")
	require.NoError(t, err)
	after := resolveApprovalCard(t, c)
	require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
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

func TestCardsApproveNormalizedRoot(t *testing.T) {
	c, before := setupApprovalCard(t, "test")
	_, err := runCLI(t, "mv", "/answer-cards", "/Café")
	require.NoError(t, err)
	_, err = runCLI(t, "cards", "approve", fmt.Sprintf("id:%d", before.ID), "--root", "/Cafe\u0301", "--yes")
	require.NoError(t, err)
	after, err := c.API().GetNode(t.Context(), &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: before.ID}})
	require.NoError(t, err)
	require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
}
