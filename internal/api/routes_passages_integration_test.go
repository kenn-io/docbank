package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

func TestPassageOutlineAndSectionRoutesAreRegistered(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ref := document.PassageRefV1{Version: document.PassageRefVersionV1,
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64), BodySHA256: strings.Repeat("d", 64),
		ByteStart: 0, ByteEnd: 1, QuoteSHA256: strings.Repeat("e", 64)}
	outlineResponse, outlineBody := do(t, ts, http.MethodPost, "/api/v1/documents/outline", nil,
		api.PassageOutlineRequest{Ref: ref})
	require.Equal(t, http.StatusServiceUnavailable, outlineResponse.StatusCode, outlineBody)
	require.Contains(t, outlineBody, `"code":"processing_unavailable"`)

	sectionResponse, sectionBody := do(t, ts, http.MethodPost, "/api/v1/passages/read-section", nil,
		api.PassageReadSectionRequest{Ref: ref, NavigationKey: strings.Repeat("f", 64)})
	require.Equal(t, http.StatusServiceUnavailable, sectionResponse.StatusCode, sectionBody)
	require.Contains(t, sectionBody, `"code":"processing_unavailable"`)
}
