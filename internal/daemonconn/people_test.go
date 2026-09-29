package daemonconn

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPeopleClientValidatesIdentityAndETag(t *testing.T) {
	const (
		survivorID = "00000000-0000-4000-8000-000000000001"
		aliasID    = "00000000-0000-4000-8000-000000000002"
	)
	person := api.Person{PersonID: survivorID, DisplayName: "Synthetic Person", Origin: "operator", State: "curated", Revision: 2,
		CreatedAt: "2026-09-28T00:00:00Z", UpdatedAt: "2026-09-28T00:00:00Z"}
	person.ReachedThroughPersonID = aliasID
	detail := api.PersonDetail{Person: person, Identities: []api.PersonIdentity{}, ExternalIdentities: []api.PersonExternalIdentity{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		switch r.URL.Path {
		case "/api/v1/people/by-id/" + aliasID:
			_ = json.MarshalWrite(w, detail)
		case "/api/v1/people":
			w.WriteHeader(http.StatusCreated)
			_ = json.MarshalWrite(w, person)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := New(server.URL, "synthetic-key")
	got, err := client.Person(t.Context(), aliasID)
	require.NoError(t, err)
	require.Equal(t, survivorID, got.PersonID)
	require.Equal(t, aliasID, got.ReachedThroughPersonID)
	created, err := client.CreatePerson(t.Context(), "Synthetic Person")
	require.NoError(t, err)
	require.Equal(t, person.PersonID, created.PersonID)

	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		_ = json.MarshalWrite(w, person)
	}))
	t.Cleanup(badServer.Close)
	_, err = New(badServer.URL, "synthetic-key").CreatePerson(t.Context(), "Synthetic Person")
	require.Error(t, err)
	require.True(t, IsResponseDecodeError(err))
}

func TestSplitPersonAllowsTwoHundredOneAssignments(t *testing.T) {
	const (
		sourceID    = "00000000-0000-4000-8000-000000000001"
		newPersonID = "00000000-0000-4000-8000-000000000002"
		operationID = "00000000-0000-4000-8000-000000000003"
	)
	assignments := make([]string, 201)
	for i := range assignments {
		assignments[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
	}
	var assignmentCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		var request api.SplitPersonRequest
		if err == nil {
			err = json.Unmarshal(payload, &request)
		}
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assignmentCount.Store(int32(len(request.AssignmentIDs)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.MarshalWrite(w, api.PersonSplitReceipt{OperationID: operationID, SourcePersonID: sourceID, NewPersonID: newPersonID, MovedIdentityIDs: []string{}, CreatedAt: "2026-09-28T00:00:00Z"})
	}))
	t.Cleanup(server.Close)

	receipt, err := New(server.URL, "synthetic-key").SplitPerson(t.Context(), sourceID, 1, api.SplitPersonRequest{
		OperationID: operationID, DisplayName: "Synthetic split", AssignmentIDs: assignments,
	})
	require.NoError(t, err)
	require.Equal(t, int32(len(assignments)), assignmentCount.Load())
	require.Equal(t, operationID, receipt.OperationID)
}
