package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// Party is a stable catalog entry for an entity or person.
type Party struct {
	PartyID     string  `json:"party_id" format:"uuid"`
	Kind        string  `json:"kind"`
	DisplayName string  `json:"display_name"`
	LegalName   string  `json:"legal_name"`
	Country     *string `json:"country" nullable:"true"`
	PersonID    *string `json:"person_id" nullable:"true"`
	Own         bool    `json:"own"`
	Revision    int64   `json:"revision"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	RetiredAt   *string `json:"retired_at" nullable:"true"`
}
type PartyPage struct {
	Items  []Party `json:"items"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}
type CreatePartyRequest struct {
	Kind        string  `json:"kind"`
	DisplayName string  `json:"display_name"`
	LegalName   string  `json:"legal_name,omitzero"`
	Country     *string `json:"country,omitempty" nullable:"true"`
	PersonID    *string `json:"person_id,omitempty" nullable:"true"`
	Own         bool    `json:"own,omitzero"`
}
type PatchPartyRequest struct {
	Kind                          *string `json:"kind,omitempty"`
	DisplayName                   *string `json:"display_name,omitempty"`
	LegalName                     *string `json:"legal_name,omitempty"`
	Country                       *string `json:"country,omitempty" nullable:"true"`
	PersonID                      *string `json:"person_id,omitempty" nullable:"true"`
	Own                           *bool   `json:"own,omitempty"`
	ClearCountry                  bool    `json:"-"`
	ClearPerson                   bool    `json:"-"`
	countryPresent, personPresent bool
}

func (p *PatchPartyRequest) UnmarshalJSON(raw []byte) error {
	type fields PatchPartyRequest
	var value fields
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for name, member := range members {
		switch name {
		case "kind", "display_name", "legal_name", "own":
			if string(member) == "null" {
				return fmt.Errorf("%s cannot be null", name)
			}
		case "country", "person_id":
		default:
			return fmt.Errorf("unknown party field %q", name)
		}
	}
	*p = PatchPartyRequest(value)
	_, p.countryPresent = members["country"]
	_, p.personPresent = members["person_id"]
	return nil
}

// MarshalJSON preserves explicit nulls for nullable party fields.
func (p PatchPartyRequest) MarshalJSON() ([]byte, error) {
	type fields PatchPartyRequest
	raw, err := json.Marshal(fields(p))
	if err != nil {
		return nil, err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if p.ClearCountry {
		members["country"] = jsontext.Value("null")
	}
	if p.ClearPerson {
		members["person_id"] = jsontext.Value("null")
	}
	return json.Marshal(members)
}

type partyOutput struct {
	ETag string `header:"ETag"`
	Body Party
}
type partyPageOutput struct{ Body PartyPage }

func fromStoreParty(p store.Party) Party {
	return Party{PartyID: p.PartyID, Kind: p.Kind, DisplayName: p.DisplayName, LegalName: p.LegalName, Country: p.Country, PersonID: p.PersonID, Own: p.Own, Revision: p.Revision, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, RetiredAt: p.RetiredAt}
}
func partyResult(p store.Party) *partyOutput {
	return &partyOutput{ETag: fmt.Sprintf("%q", fmt.Sprint(p.Revision)), Body: fromStoreParty(p)}
}

func registerPartyRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listParties", Method: http.MethodGet, Path: "/api/v1/parties", Summary: "List parties"}, func(ctx context.Context, in *struct {
		Kind           string `query:"kind"`
		Own            string `query:"own"`
		IncludeRetired bool   `query:"include_retired"`
		Limit          int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset         int    `query:"offset" minimum:"0" default:"0"`
	}) (*partyPageOutput, error) {
		var own *bool
		switch in.Own {
		case "":
		case "true":
			own = new(true)
		case "false":
			own = new(false)
		default:
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_party", "own must be true or false")
		}
		records, err := d.Store.ListParties(ctx, store.PartyListOptions{Kind: in.Kind, Own: own, IncludeRetired: in.IncludeRetired, Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &partyPageOutput{Body: PartyPage{Items: []Party{}, Limit: in.Limit, Offset: in.Offset}}
		for _, p := range records {
			out.Body.Items = append(out.Body.Items, fromStoreParty(p))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getParty", Method: http.MethodGet, Path: "/api/v1/parties/{party_id}", Summary: "Inspect a party"}, func(ctx context.Context, in *struct {
		PartyID string `path:"party_id"`
	}) (*partyOutput, error) {
		p, err := d.Store.GetParty(ctx, in.PartyID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return partyResult(p), nil
	})
	huma.Register(api, huma.Operation{OperationID: "createParty", Method: http.MethodPost, Path: "/api/v1/parties", Summary: "Create a party", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body CreatePartyRequest }) (*partyOutput, error) {
		var out *partyOutput
		err := g.mutate(ctx, func() error {
			p, err := d.Store.CreateParty(ctx, store.PartyInput{Kind: in.Body.Kind, DisplayName: in.Body.DisplayName, LegalName: in.Body.LegalName, Country: in.Body.Country, PersonID: in.Body.PersonID, Own: in.Body.Own})
			if err != nil {
				return FromStoreError(err)
			}
			out = partyResult(p)
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "updateParty", Method: http.MethodPatch, Path: "/api/v1/parties/{party_id}", Summary: "Edit a party"}, func(ctx context.Context, in *struct {
		PartyID string `path:"party_id"`
		IfMatch string `header:"If-Match"`
		Body    PatchPartyRequest
	}) (*partyOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *partyOutput
		err = g.mutate(ctx, func() error {
			p, err := d.Store.GetParty(ctx, in.PartyID)
			if err != nil {
				return FromStoreError(err)
			}
			input := store.PartyInput{Kind: p.Kind, DisplayName: p.DisplayName, LegalName: p.LegalName, Country: p.Country, PersonID: p.PersonID, Own: p.Own}
			if in.Body.Kind != nil {
				input.Kind = *in.Body.Kind
			}
			if in.Body.DisplayName != nil {
				input.DisplayName = *in.Body.DisplayName
			}
			if in.Body.LegalName != nil {
				input.LegalName = *in.Body.LegalName
			}
			if in.Body.Own != nil {
				input.Own = *in.Body.Own
			}
			if in.Body.countryPresent {
				input.Country = in.Body.Country
			}
			if in.Body.personPresent {
				input.PersonID = in.Body.PersonID
			}
			p, err = d.Store.UpdateParty(ctx, in.PartyID, revision, input)
			if err != nil {
				return FromStoreError(err)
			}
			out = partyResult(p)
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "retireParty", Method: http.MethodPost, Path: "/api/v1/parties/{party_id}/retire", Summary: "Retire a party"}, func(ctx context.Context, in *struct {
		PartyID string `path:"party_id"`
		IfMatch string `header:"If-Match"`
	}) (*partyOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *partyOutput
		err = g.mutate(ctx, func() error {
			p, err := d.Store.RetireParty(ctx, in.PartyID, revision)
			if err != nil {
				return FromStoreError(err)
			}
			out = partyResult(p)
			return nil
		})
		return out, err
	})
}
