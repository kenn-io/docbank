package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPeopleMCPWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false))
	for _, name := range []string{"get_person", "list_person_custodians"} {
		assert.Contains(t, readOnly, name)
	}
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		assert.NotContains(t, readOnly, name)
	}

	withWrites := catalogMap(toolCatalog(false, false, false, true))
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		tool := withWrites[name]
		require.NotNil(t, tool)
		require.NotNil(t, tool.Annotations)
		assert.False(t, tool.Annotations.ReadOnlyHint)
		assertSchemaContract(t, tool.InputSchema)
		assertSchemaContract(t, tool.OutputSchema)
	}
	server := newServerWithOptions(testImplementation(), ServerOptions{AllowPersonEdits: true})
	discovery := decodeResult(t, exchangeRaw(t, server, requestFor("server/discover", nil)))
	assert.Equal(t, catalogInstructions(false, false, false, true), discovery["instructions"])
}

func TestPeopleMCPWorkflow(t *testing.T) {
	const (
		survivorID  = "00000000-0000-4000-8000-000000000001"
		absorbedID  = "00000000-0000-4000-8000-000000000002"
		newPersonID = "00000000-0000-4000-8000-000000000003"
		operationID = "00000000-0000-4000-8000-000000000004"
		createdAt   = "2026-09-28T00:00:00Z"
	)
	person := api.Person{PersonID: survivorID, DisplayName: "Synthetic", Origin: "operator", State: "curated", Revision: 1, CreatedAt: createdAt, UpdatedAt: createdAt}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people":
			w.Header().Set("ETag", `"1"`)
			w.WriteHeader(http.StatusCreated)
			_ = json.MarshalWrite(w, person)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/people/by-id/"+survivorID:
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, person.Revision))
			_ = json.MarshalWrite(w, api.PersonDetail{Person: person, Identities: []api.PersonIdentity{}, ExternalIdentities: []api.PersonExternalIdentity{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/people/by-id/"+survivorID+"/custodians":
			_ = json.MarshalWrite(w, api.CustodianPage{Items: []api.CustodianAssignment{}, Total: 0})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/people/by-id/"+survivorID:
			person.Revision = 2
			person.DisplayName = "Renamed"
			w.Header().Set("ETag", `"2"`)
			_ = json.MarshalWrite(w, person)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people/by-id/"+survivorID+"/retire":
			person.Revision = 3
			person.State = "retired"
			w.Header().Set("ETag", `"3"`)
			_ = json.MarshalWrite(w, person)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people/by-id/"+survivorID+"/merge":
			w.Header().Set("ETag", `"4"`)
			_ = json.MarshalWrite(w, api.PersonMergeReceipt{MergeID: newPersonID, OperationID: operationID, SurvivorPersonID: survivorID, AbsorbedPersonID: absorbedID, AbsorbedDisplayName: "Absorbed", SurvivorRevisionBefore: 3, SurvivorRevisionAfter: 4, CreatedAt: createdAt, Moved: api.PersonMergeMovedCounts{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people/by-id/"+survivorID+"/split":
			_ = json.MarshalWrite(w, api.PersonSplitReceipt{OperationID: operationID, SourcePersonID: survivorID, NewPersonID: newPersonID, MovedIdentityIDs: []string{}, CreatedAt: createdAt})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	tools := catalogMap(toolCatalog(false, false, false, true))
	call := func(name string, raw string) {
		t.Helper()
		validator := mustResolveSchema(tools[name].OutputSchema)
		_, err := executePersonWriteTool(t.Context(), lease, name, validator, []byte(raw))
		require.NoError(t, err)
	}
	call("create_person", `{"display_name":"Synthetic"}`)
	call("rename_person", `{"person_id":"`+survivorID+`","if_match_revision":1,"display_name":"Renamed"}`)
	call("retire_person", `{"person_id":"`+survivorID+`","if_match_revision":2}`)
	call("merge_people", `{"survivor_person_id":"`+survivorID+`","survivor_revision":3,"absorbed_person_id":"`+absorbedID+`","absorbed_revision":1,"operation_id":"`+operationID+`"}`)
	call("split_person", `{"person_id":"`+survivorID+`","if_match_revision":4,"operation_id":"`+operationID+`","display_name":"Split"}`)
	_, err := getPerson(t.Context(), lease, []byte(`{"person_id":"`+survivorID+`"}`))
	require.NoError(t, err)
	_, err = listPersonCustodians(t.Context(), lease, []byte(`{"person_id":"`+survivorID+`"}`))
	require.NoError(t, err)
	require.Equal(t, int32(7), requests.Load())
}

func TestPersonWriteTreatsMalformedSuccessAsUnknown(t *testing.T) {
	const personID = "00000000-0000-4000-8000-000000000001"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.MarshalWrite(w, api.Person{PersonID: personID, DisplayName: "Synthetic", Origin: "operator", State: "curated", Revision: 1,
			CreatedAt: "2026-09-28T00:00:00Z", UpdatedAt: "2026-09-28T00:00:00Z"})
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, false, false, true))["create_person"].OutputSchema)
	_, err := executePersonWriteTool(t.Context(), lease, "create_person", validator, []byte(`{"display_name":"Synthetic"}`))
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
	assert.Equal(t, int32(1), requests.Load())
}
