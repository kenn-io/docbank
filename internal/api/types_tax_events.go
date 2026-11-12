package api

import (
	"encoding/json/v2"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// TaxEvent is a dated event; references are masked except on opted-in detail reads.
type TaxEvent struct {
	EventID        string  `json:"event_id" format:"uuid"`
	RecordID       string  `json:"record_id" format:"uuid"`
	Kind           string  `json:"kind"`
	OccurredOn     string  `json:"occurred_on"`
	SetsState      string  `json:"sets_state"`
	AmountMinor    *int64  `json:"amount_minor" nullable:"true"`
	Currency       string  `json:"currency"`
	Reference      string  `json:"reference"`
	Note           string  `json:"note"`
	RecordedAt     string  `json:"recorded_at"`
	VoidedAt       *string `json:"voided_at" nullable:"true"`
	VoidReason     string  `json:"void_reason"`
	RecordRevision int64   `json:"record_revision"`
}
type TaxEventMutation struct {
	Record TaxRecord `json:"record"`
	Event  TaxEvent  `json:"event"`
}
type TaxBalance struct {
	OutstandingMinor    *int64 `json:"outstanding_minor" nullable:"true"`
	Currency            string `json:"currency"`
	OtherCurrencyEvents int64  `json:"other_currency_events"`
}
type TaxRecordDetail struct {
	TaxRecord

	Events  []TaxEvent `json:"events"`
	Balance TaxBalance `json:"balance"`
}
type AddTaxEventRequest struct {
	Kind       string  `json:"kind"`
	OccurredOn string  `json:"occurred_on"`
	Amount     *string `json:"amount,omitzero" nullable:"true"`
	Currency   string  `json:"currency,omitzero"`
	Reference  string  `json:"reference,omitzero"`
	Note       string  `json:"note,omitzero"`
}
type AdvanceTaxRecordRequest struct {
	State      string  `json:"state"`
	OccurredOn string  `json:"occurred_on"`
	Currency   string  `json:"currency,omitzero"`
	Reference  string  `json:"reference,omitzero"`
	Note       string  `json:"note,omitzero"`
	Amount     *string `json:"amount,omitzero" nullable:"true"`
}
type VoidTaxEventRequest struct {
	Reason string `json:"reason"`
}

// UnmarshalJSON enforces exact names and no null for event text fields.
func (p *AddTaxEventRequest) UnmarshalJSON(raw []byte) error {
	if _, err := taxMembers(raw, []string{"kind", "occurred_on", "amount", "currency", "reference", taxNoteField}, "amount"); err != nil {
		return err
	}
	type fields AddTaxEventRequest
	return json.Unmarshal(raw, (*fields)(p))
}
func (p *AdvanceTaxRecordRequest) UnmarshalJSON(raw []byte) error {
	if _, err := taxMembers(raw, []string{"state", "occurred_on", "currency", "reference", taxNoteField, "amount"}, "amount"); err != nil {
		return err
	}
	type fields AdvanceTaxRecordRequest
	return json.Unmarshal(raw, (*fields)(p))
}
func (p *VoidTaxEventRequest) UnmarshalJSON(raw []byte) error {
	if _, err := taxMembers(raw, []string{"reason"}); err != nil {
		return err
	}
	type fields VoidTaxEventRequest
	return json.Unmarshal(raw, (*fields)(p))
}

func taxEventRequestSchema(schema *huma.Schema) *huma.Schema {
	for field, length := range map[string]int{"occurred_on": 10, "currency": 3, "reference": 256, taxNoteField: 8192, "amount": 32} {
		if property := schema.Properties[field]; property != nil {
			property.MaxLength = new(length)
		}
	}
	return schema
}
func (AddTaxEventRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return taxEventRequestSchema(schema)
}
func (AdvanceTaxRecordRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return taxEventRequestSchema(schema)
}
func (VoidTaxEventRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.Properties["reason"].MaxLength = new(8192)
	return schema
}
func fromStoreTaxEvent(e store.TaxEvent, reveal bool) TaxEvent {
	ref := e.Reference
	if !reveal {
		ref = maskTaxIdentifier(ref)
	}
	return TaxEvent{EventID: e.EventID, RecordID: e.RecordID, Kind: e.Kind, OccurredOn: e.OccurredOn, SetsState: e.SetsState, AmountMinor: e.AmountMinor, Currency: e.Currency, Reference: ref, Note: e.Note, RecordedAt: e.RecordedAt, VoidedAt: e.VoidedAt, VoidReason: e.VoidReason, RecordRevision: e.RecordRevision}
}
func fromStoreTaxRecordDetail(d store.TaxRecordDetail, reveal bool) TaxRecordDetail {
	out := TaxRecordDetail{TaxRecord: fromStoreTaxRecord(d.Record), Events: make([]TaxEvent, 0, len(d.Events)), Balance: TaxBalance{OutstandingMinor: d.Balance.OutstandingMinor, Currency: d.Balance.Currency, OtherCurrencyEvents: d.Balance.OtherCurrencyEvents}}
	for _, e := range d.Events {
		out.Events = append(out.Events, fromStoreTaxEvent(e, reveal))
	}
	return out
}
