package document_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestTextNavigationValidation(t *testing.T) {
	t.Parallel()
	citation := document.TextCitation{Version: 1, VaultUID: uuid.New(), NodeID: 7,
		ContentVersionID: uuid.New(), ContentSHA256: strings.Repeat("a", 64),
		RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64),
		RenditionSHA256: strings.Repeat("d", 64), End: 1}
	outline := document.TextOutlineRequest{Citation: citation}
	require.NoError(t, document.ValidateTextOutlineRequest(outline))
	outline.Offset = -1
	require.ErrorIs(t, document.ValidateTextOutlineRequest(outline), document.ErrInvalidTextNavigation)
	outline.Offset = 4098
	require.ErrorIs(t, document.ValidateTextOutlineRequest(outline), document.ErrInvalidTextNavigation)
	outline.Citation.Version = 0
	require.ErrorIs(t, document.ValidateTextOutlineRequest(outline), document.ErrInvalidTextCitation)
	valid := document.TextSectionRequest{Citation: citation,
		Section: document.TextSectionRange{Start: 0, End: 40000}, MaxChars: 16000}
	require.NoError(t, document.ValidateTextSectionRequest(valid))
	for name, change := range map[string]func(*document.TextSectionRequest){
		"negative":   func(r *document.TextSectionRequest) { r.Offset = -1 },
		"reversed":   func(r *document.TextSectionRequest) { r.Section.Start = r.Section.End },
		"bounds":     func(r *document.TextSectionRequest) { r.Section.End = 2147483648 },
		"past end":   func(r *document.TextSectionRequest) { r.Offset = 40001 },
		"zero page":  func(r *document.TextSectionRequest) { r.MaxChars = 0 },
		"large page": func(r *document.TextSectionRequest) { r.MaxChars = 16001 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			change(&bad)
			require.ErrorIs(t, document.ValidateTextSectionRequest(bad), document.ErrInvalidTextNavigation)
		})
	}
}
