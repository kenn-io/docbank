package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type ContractPartyInput struct {
	PartyID string `json:"party_id"`
	Role    string `json:"role" enum:"own,counterparty"`
}

func (p *ContractPartyInput) UnmarshalJSON(raw []byte) error {
	type fields ContractPartyInput
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for key, value := range members {
		if key != "party_id" && key != "role" {
			return fmt.Errorf("unknown field %s", key)
		}
		if string(value) == "null" {
			return fmt.Errorf("%s cannot be null", key)
		}
	}
	var value fields
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*p = ContractPartyInput(value)
	return nil
}

type ContractParty struct {
	PartyID     string  `json:"party_id"`
	Role        string  `json:"role"`
	DisplayName string  `json:"display_name"`
	Kind        string  `json:"kind"`
	Own         bool    `json:"own"`
	RetiredAt   *string `json:"retired_at" nullable:"true"`
}
type Contract struct {
	ContractID      string          `json:"contract_id"`
	Title           string          `json:"title"`
	ContractType    string          `json:"contract_type"`
	Status          string          `json:"status"`
	EffectiveStatus string          `json:"effective_status"`
	StartsOn        *string         `json:"starts_on" nullable:"true"`
	ExpiresOn       *string         `json:"expires_on" nullable:"true"`
	SignedOn        *string         `json:"signed_on" nullable:"true"`
	Summary         string          `json:"summary"`
	Purpose         string          `json:"purpose"`
	Consideration   string          `json:"consideration"`
	Termination     string          `json:"termination"`
	Revision        int64           `json:"revision"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	Parties         []ContractParty `json:"parties"`
}
type ContractPage struct {
	Items  []Contract `json:"items"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
	AsOf   string     `json:"as_of"`
}
type CreateContractRequest struct {
	Title         string               `json:"title"`
	ContractType  string               `json:"contract_type,omitzero"`
	Status        string               `json:"status,omitzero"`
	StartsOn      *string              `json:"starts_on,omitempty" nullable:"true"`
	ExpiresOn     *string              `json:"expires_on,omitempty" nullable:"true"`
	SignedOn      *string              `json:"signed_on,omitempty" nullable:"true"`
	Summary       string               `json:"summary,omitzero"`
	Purpose       string               `json:"purpose,omitzero"`
	Consideration string               `json:"consideration,omitzero"`
	Termination   string               `json:"termination,omitzero"`
	Parties       []ContractPartyInput `json:"parties,omitempty"`
}

// TransformSchema publishes the representable contract input constraints.
func (CreateContractRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return contractRequestSchema(schema)
}

func (PatchContractRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	contractRequestSchema(schema)
	for _, field := range []string{"title", "contract_type", "status", "starts_on", "expires_on", "signed_on", "summary", "purpose", "consideration", "termination"} {
		schema.AnyOf = append(schema.AnyOf, &huma.Schema{Required: []string{field}})
	}
	return schema
}

func contractRequestSchema(schema *huma.Schema) *huma.Schema {
	schema.Properties["title"].MinLength = new(1)
	schema.Properties["title"].MaxLength = new(300)
	schema.Properties["contract_type"].Pattern = `^([a-z0-9_]{1,64})?$`
	schema.Properties["status"].Enum = []any{"draft", "pending_signature", "active", "terminated", "cancelled"}
	for _, field := range []string{"starts_on", "expires_on", "signed_on"} {
		schema.Properties[field].Format = "date"
	}
	return schema
}

// UnmarshalJSON rejects ignored or null nonnullable create members.
func (p *CreateContractRequest) UnmarshalJSON(raw []byte) error {
	type fields CreateContractRequest
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for key, value := range members {
		switch key {
		case "title", "contract_type", "status", "summary", "purpose", "consideration", "termination", "parties":
			if string(value) == "null" {
				return fmt.Errorf("%s cannot be null", key)
			}
		case "starts_on", "expires_on", "signed_on", "$schema":
		default:
			return fmt.Errorf("unknown field %s", key)
		}
	}
	var value fields
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if _, ok := members["status"]; ok && value.Status == "" {
		return errors.New("status cannot be empty")
	}
	*p = CreateContractRequest(value)
	return nil
}

type PatchContractRequest struct {
	Title         *string `json:"title,omitzero" nullable:"false"`
	ContractType  *string `json:"contract_type,omitzero" nullable:"false"`
	Status        *string `json:"status,omitzero" nullable:"false"`
	StartsOn      *string `json:"starts_on,omitzero" nullable:"true"`
	ExpiresOn     *string `json:"expires_on,omitzero" nullable:"true"`
	SignedOn      *string `json:"signed_on,omitzero" nullable:"true"`
	Summary       *string `json:"summary,omitzero" nullable:"false"`
	Purpose       *string `json:"purpose,omitzero" nullable:"false"`
	Consideration *string `json:"consideration,omitzero" nullable:"false"`
	Termination   *string `json:"termination,omitzero" nullable:"false"`
	ClearStarts   bool    `json:"-"`
	ClearExpires  bool    `json:"-"`
	ClearSigned   bool    `json:"-"`
	present       map[string]jsontext.Value
}

func (p *PatchContractRequest) UnmarshalJSON(raw []byte) error {
	type fields PatchContractRequest
	var m map[string]jsontext.Value
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k, v := range m {
		switch k {
		case "title", "contract_type", "status", "summary", "purpose", "consideration", "termination":
			if string(v) == "null" {
				return fmt.Errorf("%s cannot be null", k)
			}
		case "starts_on", "expires_on", "signed_on", "$schema":
		default:
			return fmt.Errorf("unknown field %s", k)
		}
	}
	delete(m, "$schema")
	if len(m) == 0 {
		return errors.New("contract patch must contain a field")
	}
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	*p = PatchContractRequest(f)
	p.present = m
	return nil
}
func (p PatchContractRequest) MarshalJSON() ([]byte, error) {
	type fields PatchContractRequest
	raw, err := json.Marshal(fields(p))
	if err != nil {
		return nil, err
	}
	var m map[string]jsontext.Value
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if p.ClearStarts {
		m["starts_on"] = jsontext.Value("null")
	}
	if p.ClearExpires {
		m["expires_on"] = jsontext.Value("null")
	}
	if p.ClearSigned {
		m["signed_on"] = jsontext.Value("null")
	}
	return json.Marshal(m)
}

type contractOutput struct {
	ETag string `header:"ETag"`
	Body Contract
}
type contractPageOutput struct{ Body ContractPage }

func fromStoreContract(c store.Contract) Contract {
	out := Contract{ContractID: c.ContractID, Title: c.Title, ContractType: c.ContractType, Status: c.Status, EffectiveStatus: c.EffectiveStatus, StartsOn: c.StartsOn, ExpiresOn: c.ExpiresOn, SignedOn: c.SignedOn, Summary: c.Summary, Purpose: c.Purpose, Consideration: c.Consideration, Termination: c.Termination, Revision: c.Revision, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Parties: []ContractParty{}}
	for _, p := range c.Parties {
		out.Parties = append(out.Parties, ContractParty{PartyID: p.PartyID, Role: p.Role, DisplayName: p.DisplayName, Kind: p.Kind, Own: p.Own, RetiredAt: p.RetiredAt})
	}
	return out
}
func contractResult(c store.Contract) *contractOutput {
	return &contractOutput{ETag: revisionETag(c.Revision), Body: fromStoreContract(c)}
}
func contractAsOf(v string) string {
	if v == "" {
		return time.Now().Format(time.DateOnly)
	}
	return v
}
func registerContractRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listContracts", Method: http.MethodGet, Path: "/api/v1/contracts", Summary: "List contracts"}, func(ctx context.Context, in *struct {
		Status         string `query:"status"`
		Type           string `query:"type"`
		PartyID        string `query:"party_id"`
		ExpiringBefore string `query:"expiring_before"`
		Q              string `query:"q"`
		AsOf           string `query:"as_of"`
		Limit          int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset         int    `query:"offset" default:"0" minimum:"0"`
	}) (*contractPageOutput, error) {
		asOf := contractAsOf(in.AsOf)
		items, err := d.Store.ListContracts(ctx, store.ContractListOptions{Status: in.Status, ContractType: in.Type, PartyID: in.PartyID, ExpiringBefore: in.ExpiringBefore, Q: in.Q, AsOf: asOf, Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &contractPageOutput{Body: ContractPage{Items: []Contract{}, Limit: in.Limit, Offset: in.Offset, AsOf: asOf}}
		for _, c := range items {
			out.Body.Items = append(out.Body.Items, fromStoreContract(c))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getContract", Method: http.MethodGet, Path: "/api/v1/contracts/{contract_id}", Summary: "Inspect a contract"}, func(ctx context.Context, in *struct {
		ContractID string `path:"contract_id"`
		AsOf       string `query:"as_of"`
	}) (*contractOutput, error) {
		c, err := d.Store.GetContract(ctx, in.ContractID, contractAsOf(in.AsOf))
		if err != nil {
			return nil, FromStoreError(err)
		}
		return contractResult(c), nil
	})
	huma.Register(api, huma.Operation{OperationID: "createContract", Method: http.MethodPost, Path: "/api/v1/contracts", Summary: "Create a contract", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body CreateContractRequest }) (*contractOutput, error) {
		var out *contractOutput
		err := g.mutate(ctx, func() error {
			b := in.Body
			input := store.ContractInput{Title: b.Title, ContractType: b.ContractType, Status: b.Status, StartsOn: b.StartsOn, ExpiresOn: b.ExpiresOn, SignedOn: b.SignedOn, Summary: b.Summary, Purpose: b.Purpose, Consideration: b.Consideration, Termination: b.Termination}
			for _, p := range b.Parties {
				input.Parties = append(input.Parties, store.ContractPartyInput{PartyID: p.PartyID, Role: p.Role})
			}
			c, err := d.Store.CreateContract(ctx, input)
			if err != nil {
				return FromStoreError(err)
			}
			out = contractResult(c)
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "updateContract", Method: http.MethodPatch, Path: "/api/v1/contracts/{contract_id}", Summary: "Edit a contract"}, func(ctx context.Context, in *struct {
		ContractID string `path:"contract_id"`
		IfMatch    string `header:"If-Match"`
		Body       PatchContractRequest
	}) (*contractOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *contractOutput
		err = g.mutate(ctx, func() error {
			c, err := d.Store.GetContract(ctx, in.ContractID, contractAsOf(""))
			if err != nil {
				return FromStoreError(err)
			}
			if c.Revision != rev {
				return FromStoreError(store.ErrStaleRevision)
			}
			v := store.ContractInput{Title: c.Title, ContractType: c.ContractType, Status: c.Status, StartsOn: c.StartsOn, ExpiresOn: c.ExpiresOn, SignedOn: c.SignedOn, Summary: c.Summary, Purpose: c.Purpose, Consideration: c.Consideration, Termination: c.Termination}
			b := in.Body
			if b.Title != nil {
				v.Title = *b.Title
			}
			if b.ContractType != nil {
				v.ContractType = *b.ContractType
			}
			if b.Status != nil {
				v.Status = *b.Status
			}
			if b.Summary != nil {
				v.Summary = *b.Summary
			}
			if b.Purpose != nil {
				v.Purpose = *b.Purpose
			}
			if b.Consideration != nil {
				v.Consideration = *b.Consideration
			}
			if b.Termination != nil {
				v.Termination = *b.Termination
			}
			if _, ok := b.present["starts_on"]; ok {
				v.StartsOn = b.StartsOn
			}
			if _, ok := b.present["expires_on"]; ok {
				v.ExpiresOn = b.ExpiresOn
			}
			if _, ok := b.present["signed_on"]; ok {
				v.SignedOn = b.SignedOn
			}
			c, err = d.Store.UpdateContract(ctx, in.ContractID, rev, v)
			if err != nil {
				return FromStoreError(err)
			}
			out = contractResult(c)
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "deleteContract", Method: http.MethodDelete, Path: "/api/v1/contracts/{contract_id}", Summary: "Delete a contract", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, in *struct {
		ContractID string `path:"contract_id"`
		IfMatch    string `header:"If-Match"`
	}) (*struct{}, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		err = g.mutate(ctx, func() error { return FromStoreError(d.Store.DeleteContract(ctx, in.ContractID, rev)) })
		return nil, err
	})
	type linkInput struct {
		ContractID string `path:"contract_id"`
		PartyID    string `path:"party_id"`
		Role       string `query:"role" required:"true" enum:"own,counterparty"`
		IfMatch    string `header:"If-Match"`
	}
	for _, attach := range []bool{true, false} {
		method, id := http.MethodPut, "attachContractParty"
		if !attach {
			method, id = http.MethodDelete, "detachContractParty"
		}
		huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: "/api/v1/contracts/{contract_id}/parties/{party_id}", Summary: "Change a contract party"}, func(ctx context.Context, in *linkInput) (*contractOutput, error) {
			rev, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			var out *contractOutput
			err = g.mutate(ctx, func() error {
				var c store.Contract
				var err error
				if attach {
					c, err = d.Store.AttachContractParty(ctx, in.ContractID, rev, in.PartyID, in.Role)
				} else {
					c, err = d.Store.DetachContractParty(ctx, in.ContractID, rev, in.PartyID, in.Role)
				}
				if err != nil {
					return FromStoreError(err)
				}
				out = contractResult(c)
				return nil
			})
			return out, err
		})
	}
}
