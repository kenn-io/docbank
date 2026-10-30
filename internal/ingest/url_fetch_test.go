package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseURLFetchRejectsMalformedEvidence(t *testing.T) {
	const fetch = `{"version":1,"requested_url":"https://example.com/download?q=1","final_url":"https://example.com/filing.txt","fetched_at":"2026-10-01T12:34:56.123Z","etag":"V/8=","last_modified":"c2VydmVyIHN1cHBsaWVkIHRleHQ="}`
	for _, invalid := range []string{strings.Replace(fetch, `"version":1`, `"version":2`, 1), strings.Replace(fetch, `"version":1`, `"version":1,"extra":true`, 1),
		strings.Replace(fetch, "https://example.com/download?q=1", "file://host/input", 1),
		strings.Replace(fetch, "https://example.com/filing.txt", "/filing.txt", 1),
		strings.Replace(fetch, "2026-10-01T12:34:56.123Z", "yesterday", 1),
		strings.Replace(fetch, "2026-10-01T12:34:56.123Z", "2026-10-01T08:34:56.123-04:00", 1),
	} {
		assert.Nil(t, ParseURLFetch("url", invalid), invalid)
	}
}
