package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/processing"
)

func TestMediaTranscriptErrorsUseSafeHTTPDetails(t *testing.T) {
	injected := `C:\private\transcript.srt: PRIVATE_TRANSCRIPT_TEXT`
	cases := []struct {
		name, code, detail string
		err                error
		status             int
	}{
		{name: "validation", err: processing.ErrMediaTranscriptInvalid, status: http.StatusUnprocessableEntity,
			code: "validation", detail: "media transcript request is invalid"},
		{name: "oversize", err: processing.ErrMediaTranscriptOversize, status: http.StatusRequestEntityTooLarge,
			code: "media_transcript_too_large", detail: "media transcript exceeds its size limit"},
		{name: "unavailable", err: processing.ErrMediaTranscriptUnavailable, status: http.StatusServiceUnavailable,
			code: "media_transcript_unavailable", detail: "media transcript evidence is unavailable"},
		{name: "corrupt", err: processing.ErrMediaTranscriptCorrupt, status: http.StatusInternalServerError,
			code: "media_transcript_corrupt", detail: "media transcript evidence is corrupt"},
	}
	observed := make([]string, 0, len(cases))
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mapped := fromMediaError(fmt.Errorf("%w: %s", test.err, injected))
			require.Equal(t, test.status, mapped.Status)
			require.Equal(t, test.code, mapped.Code)
			require.Equal(t, test.detail, mapped.Detail)
			require.NotContains(t, mapped.Detail, `C:\private`)
			require.NotContains(t, mapped.Detail, "PRIVATE_TRANSCRIPT_TEXT")
			observed = append(observed, fmt.Sprintf("%s=%d/%s detail=%q", test.name, mapped.Status, mapped.Code, mapped.Detail))
		})
	}
	t.Logf("transcript error mapping: %s", strings.Join(observed, " "))
}
