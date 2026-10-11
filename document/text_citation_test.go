package document_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestTextCitationValidation(t *testing.T) {
	t.Parallel()
	valid := document.TextCitation{
		Version: 1, VaultUID: uuid.New(), NodeID: 7, ContentVersionID: uuid.New(),
		ContentSHA256: strings.Repeat("a", 64), RenditionAttachmentID: strings.Repeat("b", 64),
		BuildID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64), End: 16_000,
	}
	require.NoError(t, document.ValidateTextCitation(valid))
	for name, change := range map[string]func(*document.TextCitation){
		"format":         func(c *document.TextCitation) { c.Version = 2 },
		"node":           func(c *document.TextCitation) { c.NodeID = 0 },
		"nil vault":      func(c *document.TextCitation) { c.VaultUID = uuid.Nil() },
		"uuid version":   func(c *document.TextCitation) { c.ContentVersionID[6] = 0x10 },
		"uuid variant":   func(c *document.TextCitation) { c.VaultUID[8] = 0x00 },
		"content hash":   func(c *document.TextCitation) { c.ContentSHA256 = strings.Repeat("A", 64) },
		"attachment":     func(c *document.TextCitation) { c.RenditionAttachmentID = "" },
		"build":          func(c *document.TextCitation) { c.BuildID = strings.Repeat("g", 64) },
		"rendition hash": func(c *document.TextCitation) { c.RenditionSHA256 = strings.Repeat("a", 63) },
		"negative start": func(c *document.TextCitation) { c.Start = -1 },
		"empty":          func(c *document.TextCitation) { c.End = 0 },
		"reversed":       func(c *document.TextCitation) { c.Start = 3; c.End = 2 },
		"too long":       func(c *document.TextCitation) { c.End++ },
		"offset bound":   func(c *document.TextCitation) { c.Start = 2147483647; c.End = 2147483648 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			change(&bad)
			require.ErrorIs(t, document.ValidateTextCitation(bad), document.ErrInvalidTextCitation)
		})
	}
}
