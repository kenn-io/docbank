package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func TestIndexClientUsesBoundedStatusAndPlanBoundRepair(t *testing.T) {
	const fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	generation := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/index/status":
			assert.Equal(t, "true", request.URL.Query().Get("require_fresh"))
			assert.Equal(t, "1500", request.URL.Query().Get("wait_ms"))
			assert.NoError(t, json.MarshalWrite(w, processing.IndexStatusReport{
				Fresh: true, Projections: []processing.IndexProjectionStatus{{
					Target: processing.IndexTarget{Kind: processing.IndexMap}, State: processing.IndexStateDisabled,
				}},
			}))
		case "/api/v1/index/repair-plans":
			assert.Equal(t, http.MethodPost, request.Method)
			var body processing.IndexRepairPlanRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &body)) {
				return
			}
			assert.Equal(t, []processing.IndexTarget{{Kind: processing.IndexLexical}}, body.Targets)
			assert.NoError(t, json.MarshalWrite(w, processing.IndexRepairPlan{Fingerprint: fingerprint}))
		case "/api/v1/index/repairs":
			var body processing.IndexRepairRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &body)) {
				return
			}
			assert.False(t, body.AllowProviderWork)
			assert.Equal(t, fingerprint, body.PlanFingerprint)
			assert.NoError(t, json.MarshalWrite(w, processing.IndexRepairReport{
				PlanFingerprint: fingerprint, Projections: []processing.IndexRepairResult{{
					Target: processing.IndexTarget{Kind: processing.IndexLexical}, Generation: generation,
				}},
			}))
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	client := daemonconn.New(server.URL, "")

	status, err := client.IndexStatus(t.Context(), true, 1500*time.Millisecond)
	require.NoError(t, err)
	require.True(t, status.Fresh)
	plan, err := client.PlanIndexRepair(t.Context(), []processing.IndexTarget{{Kind: processing.IndexLexical}})
	require.NoError(t, err)
	require.Equal(t, fingerprint, plan.Fingerprint)
	report, err := client.RepairIndex(
		t.Context(), processing.IndexTarget{Kind: processing.IndexLexical}, fingerprint, false,
	)
	require.NoError(t, err)
	require.Equal(t, generation, report.Projections[0].Generation)
}
