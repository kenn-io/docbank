package daemonconn

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/taxrecord"
)

var eventFields = []string{"event_id", "record_id", "kind", "occurred_on", "sets_state", "amount_minor", "currency", "reference", "note", "recorded_at", "voided_at", "void_reason", "record_revision"}
var recordFields = []string{"record_id", "account_id", "filer_party_id", "authority_party_id", "kind", "title", "country", "subdivision", "tax_kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "lead_days", "state", "direction", "amount_minor", "currency", "verification", "verified_on", "verified_revision", "verification_note", "note", "revision", "created_at", "updated_at", "retired_at"}
var recordNullable = map[string]bool{"account_id": true, "authority_party_id": true, "tax_year": true, "period_start": true, "period_end": true, "due_date": true, "lead_days": true, "amount_minor": true, "verified_on": true, "verified_revision": true, "retired_at": true}

func taxMembers(raw []byte, fields []string, nullable map[string]bool) bool {
	var m map[string]jsontext.Value
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	for _, f := range fields {
		if v, ok := m[f]; !ok || (string(v) == "null" && !nullable[f]) {
			return false
		}
	}
	return true
}
func validEvent(e api.TaxEvent, r api.TaxRecord, reveal bool) bool {
	if !validUUIDv4(e.EventID) || e.RecordID != r.RecordID || e.RecordRevision < 2 || e.RecordRevision > r.Revision {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, e.RecordedAt); err != nil {
		return false
	}
	if e.VoidedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *e.VoidedAt); err != nil {
			return false
		}
		reason, err := taxrecord.NormalizeVoidReason(e.VoidReason)
		if err != nil || reason != e.VoidReason {
			return false
		}
	} else if e.VoidReason != "" {
		return false
	}
	ref := e.Reference
	if !reveal && ref != "" {
		if !strings.HasPrefix(ref, "****") || (utf8.RuneCountInString(ref) != 4 && utf8.RuneCountInString(ref) != 8) {
			return false
		}
		ref = ""
	}
	return taxrecord.ValidateEvent(r.Kind, taxrecord.EventValues{Kind: e.Kind, OccurredOn: e.OccurredOn, SetsState: e.SetsState, AmountMinor: e.AmountMinor, Currency: e.Currency, Reference: ref, Note: e.Note}) == nil
}
func invalidEventReceipt() error {
	return &responseDecodeError{err: errors.New("invalid tax event response")}
}
func (c *Connection) taxEventReceipt(ctx context.Context, response *http.Response, id, eventID string, revision int64, void bool) (api.TaxEventMutation, error) {
	var out api.TaxEventMutation
	data, err := readTaxReceipt(ctx, response, false, []string{"record", "event"}, nil)
	if err != nil {
		return out, taxMutationFailure(err, "record")
	}
	var raw map[string]jsontext.Value
	if json.Unmarshal(data, &raw) != nil || !taxMembers(raw["record"], recordFields, recordNullable) || !taxMembers(raw["event"], eventFields, map[string]bool{"amount_minor": true, "voided_at": true}) || json.Unmarshal(data, &out) != nil {
		return out, taxMutationFailure(invalidEventReceipt(), "record")
	}
	_, err = validateTaxRecordResponse(&out.Record, response, id)
	if err != nil || out.Record.Revision-1 != revision || out.Record.RetiredAt != nil || !validEvent(out.Event, out.Record, false) || (eventID != "" && out.Event.EventID != eventID) || (out.Event.VoidedAt != nil) != void || (!void && out.Event.RecordRevision != out.Record.Revision) {
		return api.TaxEventMutation{}, taxMutationFailure(invalidEventReceipt(), "record")
	}
	return out, nil
}
func (c *Connection) AdvanceTaxRecord(ctx context.Context, id string, revision int64, in api.AdvanceTaxRecordRequest) (api.TaxEventMutation, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxEventMutation{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).AdvanceTaxRecord(runtime.WithStreamingResponse(ctx), &apiclient.AdvanceTaxRecordRequestOptions{PathParams: &apiclient.AdvanceTaxRecordPath{RecordID: id}, Header: &apiclient.AdvanceTaxRecordHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxEventMutation{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	out, err := c.taxEventReceipt(ctx, response, id, "", revision, false)
	if err == nil && (out.Event.Kind != in.State || out.Record.State != in.State) {
		return api.TaxEventMutation{}, taxMutationFailure(invalidEventReceipt(), "record")
	}
	return out, err
}
func (c *Connection) AddTaxEvent(ctx context.Context, id string, revision int64, in api.AddTaxEventRequest) (api.TaxEventMutation, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxEventMutation{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).AddTaxEvent(runtime.WithStreamingResponse(ctx), &apiclient.AddTaxEventRequestOptions{PathParams: &apiclient.AddTaxEventPath{RecordID: id}, Header: &apiclient.AddTaxEventHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxEventMutation{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	out, err := c.taxEventReceipt(ctx, response, id, "", revision, false)
	if err == nil && out.Event.Kind != "note" {
		return api.TaxEventMutation{}, taxMutationFailure(invalidEventReceipt(), "record")
	}
	return out, err
}
func (c *Connection) VoidTaxEvent(ctx context.Context, id, eventID string, revision int64, in api.VoidTaxEventRequest) (api.TaxEventMutation, error) {
	if !validUUIDv4(id) || !validUUIDv4(eventID) || revision < 1 {
		return api.TaxEventMutation{}, errors.New("invalid tax record or event ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).VoidTaxEvent(runtime.WithStreamingResponse(ctx), &apiclient.VoidTaxEventRequestOptions{PathParams: &apiclient.VoidTaxEventPath{RecordID: id, EventID: eventID}, Header: &apiclient.VoidTaxEventHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxEventMutation{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	return c.taxEventReceipt(ctx, response, id, eventID, revision, true)
}
