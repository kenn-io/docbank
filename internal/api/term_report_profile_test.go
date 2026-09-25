package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/report"
)

func TestTermReportRequiresUnambiguousProfile(t *testing.T) {
	t.Parallel()
	for _, multiple := range []bool{false, true} {
		name := "sole profile"
		if multiple {
			name = "multiple profiles"
		}
		t.Run(name, func(t *testing.T) {
			var cfg config.Config
			ts, s := newTestServer(t, func(d *api.Deps) {
				renditionTextConfig(d)
				if multiple {
					second := d.Cfg.ProcessingProfiles["archive"]
					second.MaxDocumentChars = 90000
					d.Cfg.ProcessingProfiles["second"] = second
				}
				cfg = d.Cfg
			})
			hash, size, err := s.Blobs.Write(strings.NewReader("%PDF-1.4 synthetic source"))
			require.NoError(t, err)
			node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.pdf", hash, size, "application/pdf")
			require.NoError(t, err)
			publishRenditionTextFixture(t, s, cfg, node)
			request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
				Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
					Dates: report.DateRange{Start: "2020-01-01", End: "2030-12-31"}}}}
			if multiple {
				for _, mode := range []string{"strict", "available_only"} {
					request.CoverageMode = mode
					response, body := do(t, ts, http.MethodPost, "/api/v1/search-exports", nil, request)
					require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
					require.Contains(t, body, `"code":"invalid_profile"`)
				}
				history, err := s.ListTermReportHistory(t.Context(), 0, 20)
				require.NoError(t, err)
				require.Zero(t, history.Total, "ambiguous requests must not publish export history")
				request.Profile = "archive"
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/search-exports", nil, request)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var summary report.Summary
			require.NoError(t, json.Unmarshal([]byte(body), &summary))
			require.Equal(t, "complete", summary.State)
			require.Len(t, summary.Counts, 1)
			require.Equal(t, int64(1), summary.Counts[0].Hits)
			require.Zero(t, summary.Coverage.MissingText)
		})
	}
}
