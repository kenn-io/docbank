package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/report"
)

func TestTermReportRevisionHistoryWaitsForMaintenance(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		name := "persist after maintenance"
		if cancelWaiting {
			name = "cancel while waiting"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := api.NewOperationGate()
				ts, catalog := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
				ts.Close()
				createFileWithContent(t, ts, catalog, "/synthetic-alpha.txt", "synthetic alpha")
				create := httptest.NewRequest(http.MethodPost, "/api/v1/search-exports",
					strings.NewReader(`{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only",`+
						`"terms":[{"number":1,"expression":"alpha","syntax":"simple",`+
						`"dates":{"start":"2000-01-01","end":"2000-12-31"}}]}`))
				create.Header.Set("X-Api-Key", testAPIKey)
				create.Header.Set("Content-Type", "application/json")
				created := httptest.NewRecorder()
				catalog.Server.Handler().ServeHTTP(created, create)
				require.Equal(t, http.StatusOK, created.Code, created.Body.String())
				var parent report.Summary
				require.NoError(t, json.Unmarshal(created.Body.Bytes(), &parent))

				entered, release := make(chan struct{}), make(chan struct{})
				releaseMaintenance := sync.OnceFunc(func() { close(release) })
				defer releaseMaintenance()
				maintenanceDone := make(chan error, 1)
				go func() {
					maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
						close(entered)
						<-release
						return nil
					})
				}()
				<-entered
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				request := httptest.NewRequestWithContext(ctx, http.MethodPost,
					"/api/v1/search-exports/"+parent.ID+"/revisions", strings.NewReader(`{"choices":[]}`))
				request.Header.Set("X-Api-Key", testAPIKey)
				request.Header.Set("Content-Type", "application/json")
				revision := httptest.NewRecorder()
				finished := make(chan struct{})
				go func() {
					catalog.Server.Handler().ServeHTTP(revision, request)
					close(finished)
				}()
				synctest.Wait()
				select {
				case <-finished:
					t.Fatalf("revision finished while maintenance held the gate: %d %s", revision.Code, revision.Body.String())
				default:
				}
				history, err := catalog.ListTermReportHistory(t.Context(), 0, 20)
				require.NoError(t, err)
				require.Equal(t, 1, history.Total)
				if cancelWaiting {
					cancel()
					synctest.Wait()
					<-finished
					require.Equal(t, http.StatusInternalServerError, revision.Code, revision.Body.String())
					releaseMaintenance()
				} else {
					releaseMaintenance()
					<-finished
					require.Equal(t, http.StatusOK, revision.Code, revision.Body.String())
				}
				require.NoError(t, <-maintenanceDone)
				history, err = catalog.ListTermReportHistory(t.Context(), 0, 20)
				require.NoError(t, err)
				if cancelWaiting {
					require.Equal(t, 1, history.Total)
				} else {
					var summary report.Summary
					require.NoError(t, json.Unmarshal(revision.Body.Bytes(), &summary))
					require.Equal(t, parent.ID, summary.ParentID)
					require.Equal(t, 2, history.Total)
					require.ElementsMatch(t, []string{parent.ID, summary.ID},
						[]string{history.Items[0].Summary.ID, history.Items[1].Summary.ID})
				}
				readParent := httptest.NewRequest(http.MethodGet, "/api/v1/search-exports/"+parent.ID, nil)
				readParent.Header.Set("X-Api-Key", testAPIKey)
				retained := httptest.NewRecorder()
				catalog.Server.Handler().ServeHTTP(retained, readParent)
				require.Equal(t, http.StatusOK, retained.Code, retained.Body.String())
			})
		})
	}
}
