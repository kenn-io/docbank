package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const taxRecordTestReceipt = `{"record_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","account_id":null,"authority_party_id":null,"subdivision":"","form_code":"","tax_year":null,"period_start":null,"period_end":null,"due_date":null,"lead_days":null,"amount_minor":null,"currency":"","verified_on":null,"verified_revision":null,"verification_note":"","note":"","retired_at":null,"country":"ZZ","tax_kind":"Example","kind":"filing","title":"Example","state":"expected","direction":"unknown","verification":"unverified","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`

func TestTaxCLIEventRevisionNoRetry(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var reads, writes int
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			reads++
			w.Header().Set("ETag", `"1"`)
			_, _ = w.Write([]byte(strings.TrimSuffix(taxRecordTestReceipt, "}") + `,"events":[]}`))
			return
		}
		writes++
		assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
		w.WriteHeader(412)
		_, _ = w.Write([]byte(`{"status":412,"code":"stale_revision"}`))
	})
	_, err := runCLI(t, "tax", "event", "add", id, "--kind", "note", "--occurred-on", "2026-01-01", "--reference", "private-synthetic")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-synthetic")
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
}

func TestTaxCLIEventExplicitRevisionSkipsRead(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var reads, writes int
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			reads++
			return
		}
		writes++
		assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
		w.WriteHeader(412)
		_, _ = w.Write([]byte(`{"status":412,"code":"stale_revision"}`))
	})
	_, err := runCLI(t, "tax", "record", "advance", id, "--state", "filed", "--occurred-on", "2026-01-01", "--revision", "7")
	require.Error(t, err)
	assert.Equal(t, 0, reads)
	assert.Equal(t, 1, writes)
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
		record := strings.Replace(taxRecordTestReceipt, `"revision":1`, `"revision":2`, 1)
		_, _ = w.Write([]byte(strings.TrimSuffix(record, "}") + `,"events":[` + e + `]}`))
	})
	out, err := runCLI(t, "tax", "record", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "****1234")
	assert.NotContains(t, out, "private-synthetic")
	out, err = runCLI(t, "tax", "record", "show", id, "--reveal", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "private-synthetic-1234")
}
