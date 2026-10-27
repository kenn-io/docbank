package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestCardsApproveRebindCurrentRendition(t *testing.T) {
	c, before := setupApprovalCard(t, "test", "雪 prefix test suffix")
	_, old := readApprovalCard(t, c, before)
	_, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--yes")
	require.ErrorContains(t, err, "citation_not_current")
	_, err = runCLI(t, "versions", "prune", "/source.pdf", "--all-prior", "--run")
	require.NoError(t, err)
	_, err = c.ResolveTextCitation(t.Context(), old.Citations[0].TextCitation())
	require.Error(t, err)
	out, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--rebind", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "[1] /source.pdf: test")
	after := resolveApprovalCard(t, c)
	require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
	_, card := readApprovalCard(t, c, after)
	require.Equal(t, answercard.Approved, card.Effective(nil).State)
	require.NotEqual(t, old.Citations[0].ContentVersionID, card.Citations[0].ContentVersionID)
	require.NotEqual(t, old.Citations[0].ContentSHA256, card.Citations[0].ContentSHA256)
	require.NotEqual(t, old.Citations[0].RenditionAttachmentID, card.Citations[0].RenditionAttachmentID)
	require.NotEqual(t, old.Citations[0].Start, card.Citations[0].Start)
	require.Equal(t, old.Answer, card.Answer)
	resolved, err := c.ResolveTextCitation(t.Context(), card.Citations[0].TextCitation())
	require.NoError(t, err)
	require.Equal(t, "test", resolved.Text)
}

func TestCardsApproveRebindRefusesUnprovableQuote(t *testing.T) {
	for _, tc := range []struct{ quote, current, reason string }{
		{"test", "test test", "ambiguous"},
		{"test", "gone", "missing"},
		{"test", "unavailable", "rendition unavailable"},
		{"", "test", "empty"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c, before := setupApprovalCard(t, tc.quote, tc.current)
			data, _ := readApprovalCard(t, c, before)
			_, err := runCLI(t, "cards", "approve", "/answer-cards/card.md", "--rebind", "--yes")
			require.ErrorContains(t, err, tc.reason)
			after := resolveApprovalCard(t, c)
			require.Equal(t, before.CurrentVersionID, after.CurrentVersionID)
			unchanged, _ := readApprovalCard(t, c, after)
			require.Equal(t, data, unchanged)
		})
	}
}

func TestCardsApproveRebindRechecksAfterConfirmation(t *testing.T) {
	for _, change := range []string{"source", "card", "cancel"} {
		t.Run(change, func(t *testing.T) {
			c, before := setupApprovalCard(t, "test", "prefix test suffix")
			data, _ := readApprovalCard(t, c, before)
			want := "approval cancelled"
			expected := before.CurrentVersionID
			err := answercard.ApproveRebind(t.Context(), c, before, func(card answercard.Card, paths []string) (bool, error) {
				require.Equal(t, "test", card.Citations[0].Quote)
				require.Equal(t, []string{"/source.pdf"}, paths)
				switch change {
				case "source":
					file := writeSourceFile(t, "changed.pdf", "new synthetic bytes")
					_, err := runCLI(t, "put", file, "/source.pdf")
					require.NoError(t, err)
					want = "citation_not_current"
				case "card":
					file := writeSourceFile(t, "edited.md", strings.Replace(string(data), "Test answer.", "Edited answer.", 1))
					_, err := runCLI(t, "put", file, "/answer-cards/card.md")
					require.NoError(t, err)
					expected = resolveApprovalCard(t, c).CurrentVersionID
					want = "stale_revision"
				}
				return change != "cancel", nil
			})
			require.ErrorContains(t, err, want)
			c, err = daemonconn.Ensure(t.Context())
			require.NoError(t, err)
			require.Equal(t, expected, resolveApprovalCard(t, c).CurrentVersionID)
		})
	}
}

func TestCardsApproveRebindLaterCitationFailureWritesNothing(t *testing.T) {
	c, before := setupApprovalCard(t, "test", "prefix test suffix")
	data, _ := readApprovalCard(t, c, before)
	s := string(data)
	start, end := strings.Index(s, "  - version:"), strings.Index(s, "state: draft")
	require.GreaterOrEqual(t, start, 0)
	require.Greater(t, end, start)
	second := strings.Replace(s[start:end], `quote: "test"`, `quote: "fake"`, 1)
	require.NotEqual(t, s[start:end], second)
	file := writeSourceFile(t, "two.md", s[:end]+second+s[end:])
	_, err := runCLI(t, "put", file, "/answer-cards/card.md")
	require.NoError(t, err)
	before = resolveApprovalCard(t, c)
	data, _ = readApprovalCard(t, c, before)
	_, err = runCLI(t, "cards", "approve", "/answer-cards/card.md", "--rebind", "--yes")
	require.ErrorContains(t, err, "citation 1: rebind_refused: missing")
	after := resolveApprovalCard(t, c)
	require.Equal(t, before.CurrentVersionID, after.CurrentVersionID)
	unchanged, _ := readApprovalCard(t, c, after)
	require.Equal(t, data, unchanged)
}
