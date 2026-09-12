package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
)

func collectionCoverageConfig(d *api.Deps, names ...string) {
	d.Cfg.RetrievalProfiles = map[string]config.RetrievalProfileConfig{"local": {LexicalLimit: 10, VectorLimit: 10}}
	d.Cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{}
	for _, name := range names {
		d.Cfg.ProcessingProfiles[name] = config.ProcessingProfileConfig{
			Retrieval: "local", AttachmentPolicyFingerprint: strings.Repeat("a", 64), CompletenessFingerprint: strings.Repeat("b", 64),
			ConsentFingerprint: strings.Repeat("c", 64), LexicalSegmenterFingerprint: strings.Repeat("d", 64),
			MaxDocumentChars: 100000, MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizerFingerprint: strings.Repeat("e", 64), SanitizerFingerprint: strings.Repeat("f", 64), TrustBoundary: "vault",
		}
	}
}

func TestCollectionQualityHTTPConfigurationAndCoverage(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		profiles               []string
		query, state, selected string
	}{
		{name: "unconfigured", state: "unconfigured"},
		{name: "one", profiles: []string{"archive"}, state: "configured", selected: "archive"},
		{name: "multiple", profiles: []string{"first", "second"}, state: "profile_required"},
		{name: "explicit", profiles: []string{"first", "second"}, query: "?profile=second", state: "configured", selected: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestServer(t, func(d *api.Deps) { collectionCoverageConfig(d, tc.profiles...) })
			imported := importCollection(t, ts.URL, ts.Client(), "manual.txt", "synthetic manual", nil)
			for _, suffix := range []string{"", "/quality", "/members"} {
				resp, body := get(t, ts, "/api/v1/collections/"+imported.IngestID+suffix+tc.query, nil)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				var raw map[string]any
				require.NoError(t, json.Unmarshal([]byte(body), &raw))
				collection := raw
				if suffix != "" {
					var ok bool
					collection, ok = raw["collection"].(map[string]any)
					require.True(t, ok)
				}
				require.Contains(t, collection, "coverage")
				coverage, ok := collection["coverage"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.state, coverage["configuration"])
				assert.Equal(t, tc.selected, coverage["profile"])
				if tc.state == "configured" {
					require.NotNil(t, coverage["counts"])
					counts, ok := coverage["counts"].(map[string]any)
					require.True(t, ok)
					// This HTTP fixture imports bytes but starts no extraction worker.
					assert.InDelta(t, 1, counts["unprocessed"], 0)
				} else {
					assert.Nil(t, coverage["counts"])
				}
			}
			resp, body := get(t, ts, "/api/v1/collections"+tc.query, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var page map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			items, ok := page["items"].([]any)
			require.True(t, ok)
			require.Len(t, items, 1)
			item, ok := items[0].(map[string]any)
			require.True(t, ok)
			coverage, ok := item["coverage"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.state, coverage["configuration"])
		})
	}
}

func TestCollectionQualityHTTPRejectsUnknownInputsAndMissingAuth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	imported := importCollection(t, ts.URL, ts.Client(), "manual.txt", "synthetic manual", nil)
	base := "/api/v1/collections/" + imported.IngestID
	for _, suffix := range []string{"?profile=unknown", "/quality?fields=unknown", "/quality?fields=size,size"} {
		resp, body := get(t, ts, base+suffix, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body := get(t, ts, base+"/quality", map[string]string{"X-Api-Key": ""})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
}
