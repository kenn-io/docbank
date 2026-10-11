package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoTimelineRefusesCountsThatDoNotAddUp(t *testing.T) {
	t.Parallel()
	valid := func() api.PhotoCaptureDayFacet {
		return api.PhotoCaptureDayFacet{
			Dimension: "capture_day", Available: true, Total: new(int64(4)), Missing: new(int64(1)), Other: new(int64(0)),
			Values: []api.WorkspaceFacetValue{
				{Key: "2024-02-29", Label: "2024-02-29", Count: 2},
				{Key: "2023-12-31", Label: "2023-12-31", Count: 1},
			},
		}
	}
	unavailable := api.PhotoCaptureDayFacet{
		Dimension: "capture_day", Reason: "time_budget_exceeded", Values: []api.WorkspaceFacetValue{},
	}
	cases := []struct {
		name   string
		facets []api.PhotoCaptureDayFacet
		err    string
	}{
		{"complete counts", []api.PhotoCaptureDayFacet{valid()}, ""},
		{"unavailable", []api.PhotoCaptureDayFacet{unavailable}, ""},
		{"no facet", nil, "one capture_day facet"},
		{"wrong dimension", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet { f := valid(); f.Dimension = "tags"; return f }()}, "one capture_day facet"},
		{"wrong total", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet { f := valid(); f.Total = new(int64(5)); return f }()}, "do not add up"},
		{"missing counts", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet { f := valid(); f.Missing = nil; return f }()}, "lacks complete counts"},
		{"other counts", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet { f := valid(); f.Other = new(int64(1)); return f }()}, "lacks complete counts"},
		{"invalid day", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet {
			f := valid()
			f.Values[0].Key, f.Values[0].Label = "2025-02-29", "2025-02-29"
			return f
		}()}, "not a counted calendar date"},
		{"ascending days", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet {
			f := valid()
			f.Values[0], f.Values[1] = f.Values[1], f.Values[0]
			return f
		}()}, "out of descending order"},
		{"unavailable with counts", []api.PhotoCaptureDayFacet{func() api.PhotoCaptureDayFacet { f := unavailable; f.Total = new(int64(1)); return f }()}, "unavailable photo timeline reports counts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var received api.PhotoTimelineRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/photos/assets/timeline" {
					http.NotFound(w, r)
					return
				}
				if err := json.UnmarshalRead(r.Body, &received); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				body, err := json.Marshal(api.PhotoTimeline{Query: received.Query, Facets: tc.facets})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_, _ = w.Write(body)
			}))
			t.Cleanup(server.Close)
			client := New(server.URL, "synthetic-key")
			timeline, err := client.PhotoTimeline(t.Context(), api.PhotoTimelineRequest{Query: api.QueryPayload(`{}`)})
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.facets, timeline.Facets)
			require.JSONEq(t, `{}`, string(received.Query))
		})
	}
}

func TestPhotoTimelineRejectsInvalidQueryBeforeTransport(t *testing.T) {
	t.Parallel()
	client := New("http://127.0.0.1:1", "synthetic-key")
	_, err := client.PhotoTimeline(t.Context(), api.PhotoTimelineRequest{Query: api.QueryPayload(`{"unknown":true}`)})
	require.ErrorContains(t, err, "photo timeline query")
}
