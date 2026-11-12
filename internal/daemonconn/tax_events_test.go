package daemonconn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

var eventRecordFixture = strings.Replace(taxRecordFixture, `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`, 1)

const eventFixture = `{"event_id":"33333333-3333-4333-8333-333333333333","record_id":"11111111-1111-4111-8111-111111111111","kind":"note","occurred_on":"2026-01-01","sets_state":"","amount_minor":null,"currency":"","reference":"****1234","note":"Example","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":2}`

func TestTaxEventClientContracts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(eventRecordFixture, `"revision":1`, `"revision":2`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "true", r.URL.Query().Get("reveal"))
		_, _ = w.Write([]byte(strings.TrimSuffix(record, "}") + `,"events":[` + eventFixture + `],"balance":{"outstanding_minor":null,"currency":"","other_currency_events":0}}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	detail, err := c.TaxRecord(t.Context(), id, true)
	require.NoError(t, err)
	require.Len(t, detail.Events, 1)
}
func TestTaxEventClientRejectsInvalidReceipts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(eventRecordFixture, `"revision":1`, `"revision":2`, 1)
	for _, tc := range []struct{ event, etag string }{
		{strings.Replace(eventFixture, `"voided_at":null,`, "", 1), `"2"`},
		{strings.Replace(eventFixture, id, "44444444-4444-4444-8444-444444444444", 1), `"2"`},
		{strings.Replace(eventFixture, `"reference":"****1234"`, `"reference":"private-synthetic"`, 1), `"2"`},
		{eventFixture, `"3"`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", tc.etag)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"record":` + record + `,"event":` + tc.event + `}`))
		}))
		_, err := New(server.URL, "").AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "note", OccurredOn: "2026-01-01"})
		require.Error(t, err)
		assert.True(t, IsResponseDecodeError(err))
		assert.NotContains(t, err.Error(), "private-synthetic")
		server.Close()
	}
}

func TestTaxEventClientAdvanceAndVoidPaths(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const eid = "33333333-3333-4333-8333-333333333333"
	record := strings.Replace(strings.Replace(eventRecordFixture, `"revision":1`, `"revision":2`, 1), `"state":"expected"`, `"state":"filed"`, 1)
	event := strings.Replace(strings.Replace(eventFixture, `"kind":"note"`, `"kind":"filed"`, 1), `"sets_state":""`, `"sets_state":"filed"`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		result, parent := event, record
		if r.URL.Path == "/api/v1/tax/records/"+id+"/events" {
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
			result = eventFixture
			parent = strings.Replace(record, `"state":"filed"`, `"state":"expected"`, 1)
			w.WriteHeader(http.StatusCreated)
		} else if strings.HasSuffix(r.URL.Path, "/void") {
			w.Header().Set("ETag", `"3"`)
			assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
			parent = strings.Replace(strings.Replace(record, `"revision":2`, `"revision":3`, 1), `"state":"filed"`, `"state":"expected"`, 1)
			assert.Equal(t, "/api/v1/tax/records/"+id+"/events/"+eid+"/void", r.URL.Path)
			result = strings.Replace(strings.Replace(event, `"voided_at":null`, `"voided_at":"2026-01-01T00:00:00Z"`, 1), `"void_reason":""`, `"void_reason":"Correction"`, 1)
		} else {
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
			assert.Equal(t, "/api/v1/tax/records/"+id+"/advance", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"record":` + parent + `,"event":` + result + `}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	added, err := c.AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "note", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), added.Record.Revision)
	_, err = c.AdvanceTaxRecord(t.Context(), id, 1, api.AdvanceTaxRecordRequest{State: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	_, err = c.VoidTaxEvent(t.Context(), id, eid, 2, api.VoidTaxEventRequest{Reason: "Correction"})
	require.NoError(t, err)
}

func TestTaxEventVoidedTypedError(t *testing.T) {
	t.Parallel()
	const recordID = "11111111-1111-4111-8111-111111111111"
	const eventID = "33333333-3333-4333-8333-333333333333"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/tax/records/"+recordID+"/events/"+eventID+"/void", r.URL.Path)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"title":"Conflict","code":"tax_event_voided","detail":"private-synthetic"}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").VoidTaxEvent(t.Context(), recordID, eventID, 2, api.VoidTaxEventRequest{Reason: "Correction"})
	require.ErrorIs(t, err, store.ErrTaxEventVoided)
	assert.NotContains(t, err.Error(), "private-synthetic")
}

func TestTaxCashDetailRejectsWrongBalance(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	record := strings.Replace(strings.Replace(strings.Replace(strings.Replace(eventRecordFixture, `"revision":1`, `"revision":2`, 1), `"direction":"unknown"`, `"direction":"payable"`, 1), `"amount_minor":null`, `"amount_minor":123400`, 1), `"currency":""`, `"currency":"USD"`, 1)
	event := strings.Replace(strings.Replace(strings.Replace(eventFixture, `"kind":"note"`, `"kind":"payment"`, 1), `"amount_minor":null`, `"amount_minor":10000`, 1), `"currency":""`, `"currency":"USD"`, 1)
	for _, amount := range []string{"113400", "123400"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"2"`)
			_, _ = w.Write([]byte(strings.TrimSuffix(record, "}") + `,"events":[` + event + `],"balance":{"outstanding_minor":` + amount + `,"currency":"USD","other_currency_events":0}}`))
		}))
		detail, err := New(server.URL, "").TaxRecord(t.Context(), id, false)
		if amount == "113400" {
			require.NoError(t, err)
			assert.Equal(t, new(int64(113400)), detail.Balance.OutstandingMinor)
		} else {
			require.True(t, IsResponseDecodeError(err))
		}
		server.Close()
	}
}

func TestTaxCashOverflowClientDiagnostic(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"code":"invalid_tax","detail":"amount: invalid cash sum private-synthetic"}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.AddTaxEvent(t.Context(), id, 1, api.AddTaxEventRequest{Kind: "payment", OccurredOn: "2026-01-01", Currency: "USD", Amount: new("1")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount: invalid cash sum")
	assert.NotContains(t, err.Error(), "private-synthetic")
	_, err = c.UpdateTaxRecord(t.Context(), id, 1, api.PatchTaxRecordRequest{Direction: new("payable"), Amount: new("1")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount: invalid cash sum")
	assert.NotContains(t, err.Error(), "private-synthetic")
}
