package daemonconn_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionJobAdmissionRejectsInvalidStatus(t *testing.T) {
	const setID = "77000000-0000-4000-8000-000000000040"
	const jobID = "77000000-0000-4000-8000-000000000041"
	const operationID = "77000000-0000-4000-8000-000000000042"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"job_id":"` + jobID + `","set_id":"` + setID +
			`","revision":1,"revision_sha256":"` + strings.Repeat("a", 64) + `","state":"invented"}`))
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.New(server.URL, "synthetic-key").AdmitProductionJob(t.Context(), setID, 1, 1,
		api.ProductionJobAdmissionRequest{JobID: jobID, OperationID: operationID})
	require.ErrorContains(t, err, "inconsistent")
}
