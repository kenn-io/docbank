package api

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type TaxRecord struct {
	RecordID         string  `json:"record_id" format:"uuid"`
	AccountID        *string `json:"account_id" nullable:"true"`
	FilerPartyID     string  `json:"filer_party_id"`
	AuthorityPartyID *string `json:"authority_party_id" nullable:"true"`
	Kind             string  `json:"kind"`
	Title            string  `json:"title"`
	Country          string  `json:"country"`
	Subdivision      string  `json:"subdivision"`
	TaxKind          string  `json:"tax_kind"`
	FormCode         string  `json:"form_code"`
	TaxYear          *int64  `json:"tax_year" nullable:"true"`
	PeriodStart      *string `json:"period_start" nullable:"true"`
	PeriodEnd        *string `json:"period_end" nullable:"true"`
	DueDate          *string `json:"due_date" nullable:"true"`
	LeadDays         *int64  `json:"lead_days" nullable:"true"`
	State            string  `json:"state"`
	Direction        string  `json:"direction"`
	AmountMinor      *int64  `json:"amount_minor" nullable:"true"`
	Currency         string  `json:"currency"`
	Verification     string  `json:"verification"`
	VerifiedOn       *string `json:"verified_on" nullable:"true"`
	VerifiedRevision *int64  `json:"verified_revision" nullable:"true"`
	VerificationNote string  `json:"verification_note"`
	Note             string  `json:"note"`
	Revision         int64   `json:"revision"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	RetiredAt        *string `json:"retired_at" nullable:"true"`
}

// TransformSchema describes the normalized tax kind returned by the store.
func (TaxRecord) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.Properties["tax_kind"].MaxLength = new(64)
	return schema
}

type TaxRecordPage struct {
	Items  []TaxRecord `json:"items"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}
type CreateTaxRecordRequest struct {
	AccountID        *string `json:"account_id,omitzero" nullable:"true" format:"uuid"`
	FilerPartyID     *string `json:"filer_party_id,omitzero" nullable:"false" format:"uuid"`
	AuthorityPartyID *string `json:"authority_party_id,omitzero" nullable:"true" format:"uuid"`
	Kind             string  `json:"kind"`
	Title            string  `json:"title"`
	Country          *string `json:"country,omitzero"`
	Subdivision      *string `json:"subdivision,omitzero"`
	TaxKind          *string `json:"tax_kind,omitzero"`
	FormCode         string  `json:"form_code,omitzero"`
	TaxYear          *int64  `json:"tax_year,omitzero" nullable:"true"`
	PeriodStart      *string `json:"period_start,omitzero" nullable:"true"`
	PeriodEnd        *string `json:"period_end,omitzero" nullable:"true"`
	DueDate          *string `json:"due_date,omitzero" nullable:"true"`
	LeadDays         *int64  `json:"lead_days,omitzero" nullable:"true"`
	Direction        string  `json:"direction,omitzero"`
	Amount           *string `json:"amount,omitzero" nullable:"true"`
	Currency         *string `json:"currency,omitzero"`
	Note             string  `json:"note,omitzero"`
	ClearAuthority   bool    `json:"-"`
	present          map[string]bool
}
type PatchTaxRecordRequest struct {
	FilerPartyID     *string `json:"filer_party_id,omitzero" nullable:"false" format:"uuid"`
	AuthorityPartyID *string `json:"authority_party_id,omitzero" nullable:"true" format:"uuid"`
	Title            *string `json:"title,omitzero" nullable:"false"`
	Country          *string `json:"country,omitzero" nullable:"false"`
	Subdivision      *string `json:"subdivision,omitzero" nullable:"false"`
	TaxKind          *string `json:"tax_kind,omitzero" nullable:"false"`
	FormCode         *string `json:"form_code,omitzero" nullable:"false"`
	TaxYear          *int64  `json:"tax_year,omitzero" nullable:"true"`
	PeriodStart      *string `json:"period_start,omitzero" nullable:"true"`
	PeriodEnd        *string `json:"period_end,omitzero" nullable:"true"`
	DueDate          *string `json:"due_date,omitzero" nullable:"true"`
	LeadDays         *int64  `json:"lead_days,omitzero" nullable:"true"`
	Direction        *string `json:"direction,omitzero" nullable:"false"`
	Amount           *string `json:"amount,omitzero" nullable:"true"`
	Currency         *string `json:"currency,omitzero" nullable:"false"`
	Note             *string `json:"note,omitzero" nullable:"false"`
	ClearAuthority   bool    `json:"-"`
	ClearTaxYear     bool    `json:"-"`
	ClearPeriodStart bool    `json:"-"`
	ClearPeriodEnd   bool    `json:"-"`
	ClearDueDate     bool    `json:"-"`
	ClearLeadDays    bool    `json:"-"`
	ClearAmount      bool    `json:"-"`
	present          map[string]bool
}

func recordMembers(raw []byte, patch bool) (map[string]jsontext.Value, error) {
	raw = bytes.TrimSpace(raw)
	var members map[string]jsontext.Value
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("expected object")
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	delete(members, "$schema")
	if patch && len(members) == 0 {
		return nil, fmt.Errorf("empty patch")
	}
	for name, value := range members {
		nullable := false
		switch name {
		case "authority_party_id", "tax_year", "period_start", "period_end", "due_date", "lead_days", "amount":
			nullable = true
		case "account_id":
			if patch {
				return nil, fmt.Errorf("invalid field")
			}
			nullable = true
		case "kind":
			if patch {
				return nil, fmt.Errorf("invalid field")
			}
		case "filer_party_id", "title", "country", "subdivision", "tax_kind", "form_code", "direction", "currency", "note":
		default:
			return nil, fmt.Errorf("invalid field")
		}
		if string(value) == "null" && !nullable {
			return nil, fmt.Errorf("invalid field")
		}
	}
	return members, nil
}
func (p *CreateTaxRecordRequest) UnmarshalJSON(raw []byte) error {
	members, err := recordMembers(raw, false)
	if err != nil {
		return err
	}
	type fields CreateTaxRecordRequest
	var value fields
	if err = json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*p = CreateTaxRecordRequest(value)
	p.present = map[string]bool{}
	for k := range members {
		p.present[k] = true
	}
	return nil
}
func (p CreateTaxRecordRequest) MarshalJSON() ([]byte, error) {
	type fields CreateTaxRecordRequest
	raw, err := json.Marshal(fields(p))
	if err != nil {
		return nil, err
	}
	var members map[string]jsontext.Value
	if err = json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if p.ClearAuthority || (p.present["authority_party_id"] && p.AuthorityPartyID == nil) {
		members["authority_party_id"] = jsontext.Value("null")
	}
	for _, field := range []struct {
		name  string
		isNil bool
	}{
		{"account_id", p.AccountID == nil}, {"tax_year", p.TaxYear == nil}, {"period_start", p.PeriodStart == nil},
		{"period_end", p.PeriodEnd == nil}, {"due_date", p.DueDate == nil}, {"lead_days", p.LeadDays == nil}, {"amount", p.Amount == nil},
	} {
		if p.present[field.name] && field.isNil {
			members[field.name] = jsontext.Value("null")
		}
	}
	return json.Marshal(members)
}
func (p *PatchTaxRecordRequest) UnmarshalJSON(raw []byte) error {
	members, err := recordMembers(raw, true)
	if err != nil {
		return err
	}
	type fields PatchTaxRecordRequest
	var value fields
	if err = json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*p = PatchTaxRecordRequest(value)
	p.present = map[string]bool{}
	for k := range members {
		p.present[k] = true
	}
	return nil
}
func (p PatchTaxRecordRequest) MarshalJSON() ([]byte, error) {
	type fields PatchTaxRecordRequest
	raw, err := json.Marshal(fields(p))
	if err != nil {
		return nil, err
	}
	var members map[string]jsontext.Value
	if err = json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if p.ClearAuthority || (p.present["authority_party_id"] && p.AuthorityPartyID == nil) {
		members["authority_party_id"] = jsontext.Value("null")
	}
	if p.ClearTaxYear || (p.present["tax_year"] && p.TaxYear == nil) {
		members["tax_year"] = jsontext.Value("null")
	}
	if p.ClearPeriodStart || (p.present["period_start"] && p.PeriodStart == nil) {
		members["period_start"] = jsontext.Value("null")
	}
	if p.ClearPeriodEnd || (p.present["period_end"] && p.PeriodEnd == nil) {
		members["period_end"] = jsontext.Value("null")
	}
	if p.ClearDueDate || (p.present["due_date"] && p.DueDate == nil) {
		members["due_date"] = jsontext.Value("null")
	}
	if p.ClearLeadDays || (p.present["lead_days"] && p.LeadDays == nil) {
		members["lead_days"] = jsontext.Value("null")
	}
	if p.ClearAmount || (p.present["amount"] && p.Amount == nil) {
		members["amount"] = jsontext.Value("null")
	}
	return json.Marshal(members)
}
func (p CreateTaxRecordRequest) input() store.TaxRecordInput {
	return store.TaxRecordInput{AccountID: p.AccountID, FilerPartyID: p.FilerPartyID, AuthorityPartyID: p.AuthorityPartyID, Kind: p.Kind, Title: p.Title, Country: p.Country, Subdivision: p.Subdivision, TaxKind: p.TaxKind, FormCode: p.FormCode, TaxYear: p.TaxYear, PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd, DueDate: p.DueDate, LeadDays: p.LeadDays, Direction: p.Direction, Amount: p.Amount, Currency: p.Currency, Note: p.Note, AuthoritySet: p.ClearAuthority || p.present["authority_party_id"]}
}
func taxRecordRequestSchema(schema *huma.Schema) *huma.Schema {
	if kind := schema.Properties["tax_kind"]; kind != nil {
		kind.Description = "At most 256 UTF-8 bytes before normalization and 64 Unicode characters after trimming and NFC normalization."
	}
	for field, length := range map[string]int{"country": 2, "subdivision": 6, "currency": 3, "tax_kind": 256, "title": 1200, "form_code": 256, "amount": 32, "note": 8192} {
		if property := schema.Properties[field]; property != nil {
			property.MaxLength = new(length)
		}
	}
	schema.Properties["tax_year"].Minimum = new(float64(1))
	schema.Properties["tax_year"].Maximum = new(float64(9999))
	schema.Properties["lead_days"].Minimum = new(float64(0))
	schema.Properties["lead_days"].Maximum = new(float64(366))
	return schema
}
func (CreateTaxRecordRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return taxRecordRequestSchema(schema)
}
func (PatchTaxRecordRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	taxRecordRequestSchema(schema)
	for _, field := range []string{"filer_party_id", "authority_party_id", "title", "country", "subdivision", "tax_kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "lead_days", "direction", "amount", "currency", "note"} {
		schema.AnyOf = append(schema.AnyOf, &huma.Schema{Required: []string{field}})
	}
	return schema
}
