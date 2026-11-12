package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const taxRecordTestReceipt = `{"record_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","account_id":null,"authority_party_id":null,"subdivision":"","form_code":"","tax_year":null,"period_start":null,"period_end":null,"due_date":null,"lead_days":null,"amount_minor":null,"currency":"","verified_on":null,"verified_revision":null,"verification_note":"","note":"","retired_at":null,"country":"ZZ","tax_kind":"Example","kind":"filing","title":"Example","state":"expected","direction":"unknown","verification":"unverified","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`

func TestTaxCLIEventRevision(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, match string
		args        []string
		reads       int
	}{
		{"implicit", `"1"`, []string{"tax", "event", "add", id, "--kind", "note", "--occurred-on", "2026-01-01", "--reference", "private-synthetic"}, 1},
		{"explicit", `"7"`, []string{"tax", "record", "advance", id, "--state", "filed", "--occurred-on", "2026-01-01", "--revision", "7"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, writes int
			setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads++
					w.Header().Set("ETag", `"1"`)
					_, _ = w.Write([]byte(strings.TrimSuffix(taxRecordTestReceipt, "}") + `,"events":[],"balance":{"outstanding_minor":null,"currency":"","other_currency_events":0}}`))
					return
				}
				writes++
				assert.Equal(t, tc.match, r.Header.Get("If-Match"))
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"status":412,"code":"stale_revision"}`))
			})
			_, err := runCLI(t, tc.args...)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-synthetic")
			assert.Equal(t, tc.reads, reads)
			assert.Equal(t, 1, writes)
		})
	}
}

func TestTaxCLIEventReveal(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	event := `{"event_id":"33333333-3333-4333-8333-333333333333","record_id":"11111111-1111-4111-8111-111111111111","kind":"note","occurred_on":"2026-01-01","sets_state":"","amount_minor":null,"currency":"","reference":"****1234","note":"","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":2}`
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		e := event
		if r.URL.Query().Get("reveal") == "true" {
			e = strings.Replace(e, "****1234", "private-synthetic-1234", 1)
		}
		record := strings.Replace(strings.Replace(taxRecordTestReceipt, `"revision":1`, `"revision":2`, 1), `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`, 1)
		_, _ = w.Write([]byte(strings.TrimSuffix(record, "}") + `,"events":[` + e + `],"balance":{"outstanding_minor":null,"currency":"","other_currency_events":0}}`))
	})
	out, err := runCLI(t, "tax", "record", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "****1234")
	assert.Contains(t, out, "other currency events: 0")
	assert.NotContains(t, out, "private-synthetic")
	out, err = runCLI(t, "tax", "record", "show", id, "--reveal", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "private-synthetic-1234")
}

func TestTaxCLICashWorkflow(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const eid = "33333333-3333-4333-8333-333333333333"
	record := strings.Replace(strings.Replace(strings.Replace(taxRecordTestReceipt, `"direction":"unknown"`, `"direction":"payable"`, 1), `"amount_minor":null`, `"amount_minor":123400`, 1), `"currency":""`, `"currency":"USD"`, 1)
	event := `{"event_id":"` + eid + `","record_id":"` + id + `","kind":"payment","occurred_on":"2026-01-01","sets_state":"","amount_minor":10000,"currency":"USD","reference":"****1234","note":"","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":2}`
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		current := strings.Replace(strings.Replace(record, `"revision":1`, `"revision":2`, 1), `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`, 1)
		if r.Method == http.MethodPost {
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"record":` + current + `,"event":` + event + `}`))
			return
		}
		_, _ = w.Write([]byte(strings.TrimSuffix(current, "}") + `,"events":[` + event + `],"balance":{"outstanding_minor":113400,"currency":"USD","other_currency_events":0}}`))
	})
	out, err := runCLI(t, "tax", "event", "add", id, "--kind", "payment", "--occurred-on", "2026-01-01", "--amount", "100.00", "--currency", "USD", "--revision", "1")
	require.NoError(t, err)
	assert.NotContains(t, out, "outstanding minor")
	out, err = runCLI(t, "tax", "record", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "outstanding minor: 113400")
	assert.Less(t, strings.Index(out, id), strings.Index(out, "outstanding minor"))
}

func TestTaxCLICashRefundVoidAndPrivacy(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const payID = "33333333-3333-4333-8333-333333333333"
	const refundID = "44444444-4444-4444-8444-444444444444"
	const secret = "synthetic-private-1234"
	record := strings.Replace(strings.Replace(strings.Replace(taxRecordTestReceipt, `"direction":"unknown"`, `"direction":"payable"`, 1), `"amount_minor":null`, `"amount_minor":123400`, 1), `"currency":""`, `"currency":"USD"`, 1)
	payment := `{"event_id":"` + payID + `","record_id":"` + id + `","kind":"payment","occurred_on":"2026-01-01","sets_state":"","amount_minor":10000,"currency":"USD","reference":"****1234","note":"","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":2}`
	refund := `{"event_id":"` + refundID + `","record_id":"` + id + `","kind":"refund","occurred_on":"2026-01-01","sets_state":"","amount_minor":100,"currency":"EUR","reference":"","note":"","recorded_at":"2026-01-01T00:00:00Z","voided_at":null,"void_reason":"","record_revision":3}`
	revision := 1
	voided := false
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			assert.Equal(t, fmt.Sprintf(`"%d"`, revision), r.Header.Get("If-Match"))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var event string
			switch revision {
			case 1:
				assert.Contains(t, string(body), `"kind":"payment"`)
				assert.Contains(t, string(body), `"amount":"100.00"`)
				event = payment
			case 2:
				assert.Contains(t, string(body), `"kind":"refund"`)
				assert.Contains(t, string(body), `"currency":"EUR"`)
				event = refund
			case 3:
				assert.True(t, strings.HasSuffix(r.URL.Path, "/events/"+payID+"/void"))
				event = strings.Replace(strings.Replace(payment, `"voided_at":null`, `"voided_at":"2026-01-01T00:00:00Z"`, 1), `"void_reason":""`, `"void_reason":"correction"`, 1)
				voided = true
			default:
				t.Fatal("unexpected write")
			}
			revision++
			parent := strings.Replace(strings.Replace(record, `"revision":1`, fmt.Sprintf(`"revision":%d`, revision), 1), `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`, 1)
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, revision))
			if !voided {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = w.Write([]byte(`{"record":` + parent + `,"event":` + event + `}`))
			return
		}
		parent := strings.Replace(strings.Replace(record, `"revision":1`, fmt.Sprintf(`"revision":%d`, revision), 1), `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`, 1)
		event, amount := payment, 113400
		if voided {
			event = strings.Replace(strings.Replace(event, `"voided_at":null`, `"voided_at":"2026-01-01T00:00:00Z"`, 1), `"void_reason":""`, `"void_reason":"correction"`, 1)
			amount = 123400
		}
		if r.URL.Query().Get("reveal") == "true" {
			event = strings.Replace(event, "****1234", secret, 1)
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, revision))
		_, _ = w.Write([]byte(strings.TrimSuffix(parent, "}") + `,"events":[` + event + `,` + refund + `],"balance":{"outstanding_minor":` + fmt.Sprint(amount) + `,"currency":"USD","other_currency_events":1}}`))
	})
	out, err := runCLI(t, "tax", "event", "add", id, "--kind", "payment", "--occurred-on", "2026-01-01", "--amount", "100.00", "--currency", "USD", "--reference", secret, "--revision", "1")
	require.NoError(t, err)
	assert.NotContains(t, out, secret)
	out, err = runCLI(t, "tax", "event", "add", id, "--kind", "refund", "--occurred-on", "2026-01-01", "--amount", "1.00", "--currency", "EUR", "--revision", "2")
	require.NoError(t, err)
	assert.NotContains(t, out, secret)
	out, err = runCLI(t, "tax", "record", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "outstanding minor: 113400")
	assert.Contains(t, out, "other currency events: 1")
	assert.NotContains(t, out, secret)
	out, err = runCLI(t, "tax", "record", "show", id, "--reveal", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, secret)
	out, err = runCLI(t, "tax", "event", "void", id, payID, "--reason", "correction", "--revision", "3")
	require.NoError(t, err)
	assert.NotContains(t, out, secret)
	out, err = runCLI(t, "tax", "record", "show", id, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"outstanding_minor":123400`)
	assert.NotContains(t, out, secret)
}
