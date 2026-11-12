package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type contractDocumentLinkInput struct {
	ContractID       string `path:"contract_id"`
	NodeID           int64  `path:"node_id" minimum:"1"`
	Role             string `query:"role" required:"true" enum:"draft,signed,audit_trail,amendment,attachment"`
	ContentVersionID string `query:"content_version_id"`
	IfMatch          string `header:"If-Match"`
	emptyVersion     bool
}

func (in *contractDocumentLinkInput) Resolve(ctx huma.Context) []error {
	values, err := url.ParseQuery(ctx.URL().RawQuery)
	in.emptyVersion = err != nil || (len(values["content_version_id"]) > 0 && values["content_version_id"][0] == "")
	return nil
}

func registerContractDocumentRoutes(api huma.API, d Deps, g *gate) {
	for _, link := range []bool{true, false} {
		method, operation := http.MethodPut, "linkContractDocument"
		if !link {
			method, operation = http.MethodDelete, "unlinkContractDocument"
		}
		huma.Register(api, huma.Operation{OperationID: operation, Method: method, Path: "/api/v1/contracts/{contract_id}/documents/{node_id}", Summary: "Change a contract document"}, func(ctx context.Context, in *contractDocumentLinkInput) (*contractOutput, error) {
			rev, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			if in.emptyVersion {
				return nil, FromStoreError(fmt.Errorf("%w: content version ID must not be empty", store.ErrInvalidContract))
			}
			var out *contractOutput
			err = g.mutate(ctx, func() error {
				var c store.Contract
				var e error
				if link {
					c, e = d.Store.LinkContractDocument(ctx, in.ContractID, rev, in.NodeID, in.Role, in.ContentVersionID)
				} else {
					c, e = d.Store.UnlinkContractDocument(ctx, in.ContractID, rev, in.NodeID, in.Role, in.ContentVersionID)
				}
				if e != nil {
					return FromStoreError(e)
				}
				out = contractResult(c)
				return nil
			})
			return out, err
		})
	}
	huma.Register(api, huma.Operation{OperationID: "listNodeContracts", Method: http.MethodGet, Path: "/api/v1/nodes/{node_id}/contracts", Summary: "List contracts linked to a node"}, func(ctx context.Context, in *struct {
		NodeID int64  `path:"node_id" minimum:"1"`
		AsOf   string `query:"as_of"`
		Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int    `query:"offset" default:"0" minimum:"0"`
	}) (*contractPageOutput, error) {
		asOf := contractAsOf(in.AsOf)
		items, err := d.Store.ListContracts(ctx, store.ContractListOptions{NodeID: in.NodeID, AsOf: asOf, Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &contractPageOutput{Body: ContractPage{Items: []Contract{}, Limit: in.Limit, Offset: in.Offset, AsOf: asOf}}
		for _, c := range items {
			out.Body.Items = append(out.Body.Items, fromStoreContract(c))
		}
		return out, nil
	})
}
