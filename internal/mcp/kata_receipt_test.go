package mcp

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataclient"
)

func TestKataReceiptBoundsExistingIssueTitle(t *testing.T) {
	issue := kataclient.Issue{UID: "01SYNTHETIC", QualifiedRef: "docbank#abcd", Title: strings.Repeat("🙂", 600)}
	got := kataReceipt(issue)
	require.LessOrEqual(t, utf8.RuneCountInString(got.Title), 512)
	require.True(t, strings.HasSuffix(got.Title, "…"))
	require.Equal(t, issue.UID, got.UID)
	require.Equal(t, issue.QualifiedRef, got.QualifiedRef)
}
