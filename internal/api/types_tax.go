package api

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// TaxAccount is a tax registration. Identifiers are masked except on explicit reveal.
type TaxAccount struct {
	AccountID        string  `json:"account_id" format:"uuid"`
	FilerPartyID     string  `json:"filer_party_id"`
	AuthorityPartyID *string `json:"authority_party_id" nullable:"true"`
	Country          string  `json:"country"`
	Subdivision      string  `json:"subdivision"`
	TaxKind          string  `json:"tax_kind"`
	Identifier       string  `json:"identifier"`
	Cadence          string  `json:"cadence"`
	PeriodNote       string  `json:"period_note"`
	DueNote          string  `json:"due_note"`
	LeadDays         int64   `json:"lead_days"`
	Currency         string  `json:"currency"`
	EffectiveStart   *string `json:"effective_start" nullable:"true"`
	EffectiveEnd     *string `json:"effective_end" nullable:"true"`
	Note             string  `json:"note"`
	Revision         int64   `json:"revision"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	RetiredAt        *string `json:"retired_at" nullable:"true"`
}

type TaxAccountPage struct {
	Items  []TaxAccount `json:"items"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}
type TaxAccountListOptions struct {
	FilerPartyID, Country, TaxKind string
	IncludeRetired                 bool
	Limit, Offset                  int
}

type CreateTaxAccountRequest struct {
	FilerPartyID     string  `json:"filer_party_id"`
	AuthorityPartyID *string `json:"authority_party_id,omitempty" nullable:"true"`
	Country          string  `json:"country"`
	Subdivision      string  `json:"subdivision,omitzero"`
	TaxKind          string  `json:"tax_kind"`
	Identifier       string  `json:"identifier,omitzero"`
	Cadence          string  `json:"cadence"`
	PeriodNote       string  `json:"period_note,omitzero"`
	DueNote          string  `json:"due_note,omitzero"`
	LeadDays         *int64  `json:"lead_days,omitempty"`
	Currency         string  `json:"currency"`
	EffectiveStart   *string `json:"effective_start,omitempty" nullable:"true"`
	EffectiveEnd     *string `json:"effective_end,omitempty" nullable:"true"`
	Note             string  `json:"note,omitzero"`
}

type PatchTaxAccountRequest struct {
	Identifier               *string `json:"identifier,omitzero" nullable:"false"`
	Cadence                  *string `json:"cadence,omitzero" nullable:"false"`
	PeriodNote               *string `json:"period_note,omitzero" nullable:"false"`
	DueNote                  *string `json:"due_note,omitzero" nullable:"false"`
	LeadDays                 *int64  `json:"lead_days,omitzero" nullable:"false"`
	Currency                 *string `json:"currency,omitzero" nullable:"false"`
	EffectiveStart           *string `json:"effective_start,omitzero" nullable:"true"`
	EffectiveEnd             *string `json:"effective_end,omitzero" nullable:"true"`
	Note                     *string `json:"note,omitzero" nullable:"false"`
	ClearEffectiveStart      bool    `json:"-"`
	ClearEffectiveEnd        bool    `json:"-"`
	startPresent, endPresent bool
}

// TransformSchema requires at least one editable account field.
func (PatchTaxAccountRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	for _, field := range []string{"identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", "note"} {
		schema.AnyOf = append(schema.AnyOf, &huma.Schema{Required: []string{field}})
	}
	return schema
}

func taxAccountMembers(raw []byte, patch bool) (map[string]jsontext.Value, error) {
	raw = bytes.TrimSpace(raw)
	var members map[string]jsontext.Value
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("expected object")
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if patch && len(members) == 0 {
		return nil, fmt.Errorf("empty patch")
	}
	for name, value := range members {
		nullable := false
		switch name {
		case "effective_start", "effective_end":
			nullable = true
		case "identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "note":
		case "authority_party_id":
			if patch {
				return nil, fmt.Errorf("invalid field")
			}
			nullable = true
		case "filer_party_id", "country", "subdivision", "tax_kind":
			if patch {
				return nil, fmt.Errorf("invalid field")
			}
		default:
			return nil, fmt.Errorf("invalid field")
		}
		if string(value) == "null" && !nullable {
			return nil, fmt.Errorf("invalid field")
		}
	}
	return members, nil
}
func (p *CreateTaxAccountRequest) UnmarshalJSON(raw []byte) error {
	if _, err := taxAccountMembers(raw, false); err != nil {
		return err
	}
	type fields CreateTaxAccountRequest
	return json.Unmarshal(raw, (*fields)(p))
}
func (p *PatchTaxAccountRequest) UnmarshalJSON(raw []byte) error {
	members, err := taxAccountMembers(raw, true)
	if err != nil {
		return err
	}
	type fields PatchTaxAccountRequest
	var value fields
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*p = PatchTaxAccountRequest(value)
	_, p.startPresent = members["effective_start"]
	_, p.endPresent = members["effective_end"]
	return nil
}
func (p PatchTaxAccountRequest) MarshalJSON() ([]byte, error) {
	type fields PatchTaxAccountRequest
	raw, err := json.Marshal(fields(p))
	if err != nil {
		return nil, err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if p.ClearEffectiveStart || (p.startPresent && p.EffectiveStart == nil) {
		members["effective_start"] = jsontext.Value("null")
	}
	if p.ClearEffectiveEnd || (p.endPresent && p.EffectiveEnd == nil) {
		members["effective_end"] = jsontext.Value("null")
	}
	return json.Marshal(members)
}
func (p CreateTaxAccountRequest) input() store.TaxAccountInput {
	return store.TaxAccountInput{FilerPartyID: p.FilerPartyID, AuthorityPartyID: p.AuthorityPartyID, Country: p.Country, Subdivision: p.Subdivision, TaxKind: p.TaxKind, Identifier: p.Identifier, Cadence: p.Cadence, PeriodNote: p.PeriodNote, DueNote: p.DueNote, LeadDays: p.LeadDays, Currency: p.Currency, EffectiveStart: p.EffectiveStart, EffectiveEnd: p.EffectiveEnd, Note: p.Note}
}
