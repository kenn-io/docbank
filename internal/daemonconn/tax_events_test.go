package daemonconn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const eventFixture = `{"event_id":"33333333-3333-4333-8333-333333333333","record_id":"11111111-1111-4111-8111-111111111111","kind":"note","occurred_on":"2026-01-01","sets_state":"","amount_minor":null,"currency":"","reference":"****1234","note":"Example","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":2}`

func TestTaxEventClientContracts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(taxRecordFixture, `"revision":1`, `"revision":2`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		if r.Method == http.MethodGet {
			assert.Equal(t, "true", r.URL.Query().Get("reveal"))
			_, _ = w.Write([]byte(strings.TrimSuffix(record, "}") + `,"events":[` + eventFixture + `]}`))
			return
		}
		assert.Equal(t, "/api/v1/tax/records/"+id+"/events", r.URL.Path)
		assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"record":` + record + `,"event":` + eventFixture + `}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	detail, err := c.TaxRecord(t.Context(), id, true)
	require.NoError(t, err)
	require.Len(t, detail.Events, 1)
	result, err := c.AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "note", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Record.Revision)
}
func TestTaxEventClientRejectsMissingFields(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(taxRecordFixture, `"revision":1`, `"revision":2`, 1)
	for _, event := range []string{strings.Replace(eventFixture, `"voided_at":null,`, "", 1), strings.Replace(eventFixture, id, "invalid", 1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"2"`)
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"record":` + record + `,"event":` + event + `}`))
		}))
		_, err := New(server.URL, "").AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "note", OccurredOn: "2026-01-01"})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "33333333")
		server.Close()
	}
}

func TestTaxEventClientIdentityAndETag(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(taxRecordFixture, `"revision":1`, `"revision":2`, 1)
	for _, tc := range []struct{ event, etag string }{
		{eventFixture, `"3"`},
		{strings.Replace(eventFixture, id, "44444444-4444-4444-8444-444444444444", 1), `"2"`},
		{strings.Replace(eventFixture, `"reference":"****1234"`, `"reference":"private-synthetic"`, 1), `"2"`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", tc.etag)
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"record":` + record + `,"event":` + tc.event + `}`))
		}))
		_, err := New(server.URL, "").AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "note", OccurredOn: "2026-01-01"})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-synthetic")
		server.Close()
	}
}

func TestTaxEventClientAdvanceAndVoidPaths(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const eid = "33333333-3333-4333-8333-333333333333"
	record := strings.Replace(strings.Replace(taxRecordFixture, `"revision":1`, `"revision":2`, 1), `"state":"expected"`, `"state":"filed"`, 1)
	event := strings.Replace(strings.Replace(eventFixture, `"kind":"note"`, `"kind":"filed"`, 1), `"sets_state":""`, `"sets_state":"filed"`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
		result := event
		if strings.HasSuffix(r.URL.Path, "/void") {
			assert.Equal(t, "/api/v1/tax/records/"+id+"/events/"+eid+"/void", r.URL.Path)
			result = strings.Replace(strings.Replace(event, `"voided_at":null`, `"voided_at":"2026-01-01T00:00:00Z"`, 1), `"void_reason":""`, `"void_reason":"Correction"`, 1)
		} else {
			assert.Equal(t, "/api/v1/tax/records/"+id+"/advance", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"record":` + record + `,"event":` + result + `}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.AdvanceTaxRecord(t.Context(), id, 1, api.AdvanceTaxRecordRequest{State: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	_, err = c.VoidTaxEvent(t.Context(), id, eid, 1, api.VoidTaxEventRequest{Reason: "Correction"})
	require.NoError(t, err)
}
