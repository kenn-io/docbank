package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

const taxNoteField = "note"

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

// TransformSchema describes the normalized tax kind returned by the store.
func (TaxAccount) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.Properties["tax_kind"].MaxLength = new(64)
	return schema
}

type TaxAccountPage struct {
	Items  []TaxAccount `json:"items"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}
type CreateTaxAccountRequest struct {
	FilerPartyID     string  `json:"filer_party_id" format:"uuid"`
	AuthorityPartyID *string `json:"authority_party_id,omitempty" nullable:"true" format:"uuid"`
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

func taxAccountRequestSchema(schema *huma.Schema) *huma.Schema {
	// Raw input can include whitespace or decomposed Unicode that normalizes to
	// at most 64 runes. JSON Schema lengths count runes; store limits also count bytes.
	if kind := schema.Properties["tax_kind"]; kind != nil {
		kind.Description = "At most 256 UTF-8 bytes before normalization and 64 Unicode characters after trimming and NFC normalization."
	}
	schema.Properties["cadence"].Enum = nil
	for _, cadence := range taxrecord.AccountCadences() {
		schema.Properties["cadence"].Enum = append(schema.Properties["cadence"].Enum, cadence)
	}
	schema.Properties["lead_days"].Minimum = new(float64(0))
	schema.Properties["lead_days"].Maximum = new(float64(366))
	for field, length := range map[string]int{"country": 2, "subdivision": 6, "currency": 3, "tax_kind": 256, "identifier": 256, "period_note": 8192, "due_note": 8192, taxNoteField: 8192} {
		if property := schema.Properties[field]; property != nil {
			property.MaxLength = new(length)
			if field == "identifier" || field == "period_note" || field == "due_note" || field == taxNoteField {
				property.Description = "The limit also applies to UTF-8 bytes."
			}
		}
	}
	return schema
}

func (CreateTaxAccountRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return taxAccountRequestSchema(schema)
}

var (
	taxPatchMembers  = []string{"identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", taxNoteField}
	taxCreateMembers = append([]string{"filer_party_id", "authority_party_id", "country", "subdivision", "tax_kind"}, taxPatchMembers...)
)

// TransformSchema documents that at least one editable account field is required.
func (PatchTaxAccountRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	taxAccountRequestSchema(schema)
	for _, field := range taxPatchMembers {
		schema.AnyOf = append(schema.AnyOf, &huma.Schema{Required: []string{field}})
	}
	return schema
}

// taxMembers enforces admission that pinned Huma does not: it matches member
// names case-insensitively and skips nulls on optional properties (D46).
// Errors are fixed strings; the tax formatter returns only generic validation.
func taxMembers(raw []byte, names []string, nullable ...string) (map[string]jsontext.Value, error) {
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	delete(members, "$schema")
	for name, member := range members {
		if !slices.Contains(names, name) {
			return nil, errors.New("invalid tax field")
		}
		if string(member) == jsonNull && !slices.Contains(nullable, name) {
			return nil, errors.New("tax field cannot be null")
		}
	}
	return members, nil
}

func (p *CreateTaxAccountRequest) UnmarshalJSON(raw []byte) error {
	if _, err := taxMembers(raw, taxCreateMembers, "authority_party_id", "effective_start", "effective_end"); err != nil {
		return err
	}
	type fields CreateTaxAccountRequest
	return json.Unmarshal(raw, (*fields)(p))
}

func (p *PatchTaxAccountRequest) UnmarshalJSON(raw []byte) error {
	members, err := taxMembers(raw, taxPatchMembers, "effective_start", "effective_end")
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return errors.New("tax patch must contain a field")
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
		members["effective_start"] = jsontext.Value(jsonNull)
	}
	if p.ClearEffectiveEnd || (p.endPresent && p.EffectiveEnd == nil) {
		members["effective_end"] = jsontext.Value(jsonNull)
	}
	return json.Marshal(members)
}
func (p CreateTaxAccountRequest) input() store.TaxAccountInput {
	return store.TaxAccountInput{FilerPartyID: p.FilerPartyID, AuthorityPartyID: p.AuthorityPartyID, Country: p.Country, Subdivision: p.Subdivision, TaxKind: p.TaxKind, Identifier: p.Identifier, Cadence: p.Cadence, PeriodNote: p.PeriodNote, DueNote: p.DueNote, LeadDays: p.LeadDays, Currency: p.Currency, EffectiveStart: p.EffectiveStart, EffectiveEnd: p.EffectiveEnd, Note: p.Note}
}
