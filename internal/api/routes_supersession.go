package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type SupersessionLink struct {
	SuccessorID   int64  `json:"successor_id"`
	PredecessorID int64  `json:"predecessor_id"`
	Note          string `json:"note"`
	RecordedAt    string `json:"recorded_at"`
}
type NodeValidity struct {
	Node          Node               `json:"node"`
	AsOf          string             `json:"as_of"`
	State         string             `json:"state" enum:"current,superseded,expired,not_yet_effective"`
	Undated       bool               `json:"undated"`
	EffectiveFrom string             `json:"effective_from,omitempty"`
	EffectiveTo   string             `json:"effective_to,omitempty"`
	Predecessors  []SupersessionLink `json:"predecessors"`
	Successors    []SupersessionLink `json:"successors"`
}
type ValidityReceipt struct {
	Node    Node `json:"node"`
	Changed bool `json:"changed"`
}
type SupersessionRequest struct {
	Note string `json:"note,omitempty"`
}
type EffectiveWindowRequest struct {
	EffectiveFrom string `json:"effective_from,omitempty"`
	EffectiveTo   string `json:"effective_to,omitempty"`
}
type validityReceiptOutput struct {
	ETag string `header:"ETag"`
	Body ValidityReceipt
}
type nodeValidityOutput struct{ Body NodeValidity }

func validityReceipt(change store.SupersessionChange) *validityReceiptOutput {
	node := nodeOutputAt(change.Node, change.Path)
	return &validityReceiptOutput{ETag: node.ETag, Body: ValidityReceipt{Node: node.Body, Changed: change.Changed}}
}
func validityLinks(links []store.Supersession) []SupersessionLink {
	out := make([]SupersessionLink, 0, len(links))
	for _, l := range links {
		out = append(out, SupersessionLink{SuccessorID: l.SuccessorID, PredecessorID: l.PredecessorID, Note: l.Note, RecordedAt: l.RecordedAt})
	}
	return out
}

type supersessionInput struct {
	ID            int64  `path:"id" minimum:"1"`
	PredecessorID int64  `path:"predecessor_id" minimum:"1"`
	IfMatch       string `header:"If-Match"`
}

type effectiveWindowInput struct {
	ID      int64  `path:"id" minimum:"1"`
	IfMatch string `header:"If-Match"`
}

func registerSupersessionRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "putSupersession", Method: http.MethodPut, Path: "/api/v1/nodes/{id}/supersedes/{predecessor_id}", Summary: "Declare a document replacement", Description: "If-Match guards the predecessor's revision.", MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *struct {
		ID            int64  `path:"id" minimum:"1"`
		PredecessorID int64  `path:"predecessor_id" minimum:"1"`
		IfMatch       string `header:"If-Match"`
		Body          SupersessionRequest
	}) (*validityReceiptOutput, error) {
		return changeSupersession(ctx, d, g, supersessionInput{ID: in.ID, PredecessorID: in.PredecessorID, IfMatch: in.IfMatch}, &in.Body)
	})
	huma.Register(api, huma.Operation{OperationID: "deleteSupersession", Method: http.MethodDelete, Path: "/api/v1/nodes/{id}/supersedes/{predecessor_id}", Summary: "Remove a document replacement", Description: "If-Match guards the predecessor's revision."}, func(ctx context.Context, in *supersessionInput) (*validityReceiptOutput, error) {
		return changeSupersession(ctx, d, g, *in, nil)
	})
	huma.Register(api, huma.Operation{OperationID: "putEffectiveWindow", Method: http.MethodPut, Path: "/api/v1/nodes/{id}/effective-window", Summary: "Set a document effective window", Description: "Dates are inclusive calendar dates; If-Match guards the node revision.", MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id" minimum:"1"`
		IfMatch string `header:"If-Match"`
		Body    EffectiveWindowRequest
	}) (*validityReceiptOutput, error) {
		return changeEffectiveWindow(ctx, d, g, effectiveWindowInput{ID: in.ID, IfMatch: in.IfMatch}, &in.Body)
	})
	huma.Register(api, huma.Operation{OperationID: "deleteEffectiveWindow", Method: http.MethodDelete, Path: "/api/v1/nodes/{id}/effective-window", Summary: "Clear a document effective window", Description: "If-Match guards the node revision."}, func(ctx context.Context, in *effectiveWindowInput) (*validityReceiptOutput, error) {
		return changeEffectiveWindow(ctx, d, g, *in, nil)
	})
	huma.Register(api, huma.Operation{OperationID: "getNodeValidity", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/validity", Summary: "Inspect declared document validity"}, func(ctx context.Context, in *struct {
		ID   int64  `path:"id" minimum:"1"`
		AsOf string `query:"as_of" doc:"Evaluation date (YYYY-MM-DD); defaults to the daemon's local calendar date."`
	}) (*nodeValidityOutput, error) {
		v, err := d.Store.NodeValidity(ctx, in.ID, in.AsOf)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &nodeValidityOutput{Body: NodeValidity{Node: nodeOutputAt(v.Node, v.Path).Body, AsOf: v.AsOf, State: string(v.State), Undated: v.Undated, EffectiveFrom: v.Window.From, EffectiveTo: v.Window.To, Predecessors: validityLinks(v.Predecessors), Successors: validityLinks(v.Successors)}}, nil
	})
}

func changeSupersession(ctx context.Context, d Deps, g *gate, in supersessionInput, body *SupersessionRequest) (*validityReceiptOutput, error) {
	rev, err := parseIfMatch(in.IfMatch)
	if err != nil {
		return nil, err
	}
	var out *validityReceiptOutput
	err = g.mutate(func() error {
		var change store.SupersessionChange
		var err error
		if body != nil {
			change, err = d.Store.PutSupersession(ctx, in.ID, in.PredecessorID, rev, body.Note)
		} else {
			change, err = d.Store.DeleteSupersession(ctx, in.ID, in.PredecessorID, rev)
		}
		if err != nil {
			return FromStoreError(err)
		}
		out = validityReceipt(change)
		return nil
	})
	return out, err
}

func changeEffectiveWindow(ctx context.Context, d Deps, g *gate, in effectiveWindowInput, body *EffectiveWindowRequest) (*validityReceiptOutput, error) {
	rev, err := parseIfMatch(in.IfMatch)
	if err != nil {
		return nil, err
	}
	var out *validityReceiptOutput
	err = g.mutate(func() error {
		var change store.SupersessionChange
		var err error
		if body != nil {
			change, err = d.Store.PutEffectiveWindow(ctx, in.ID, rev, body.EffectiveFrom, body.EffectiveTo)
		} else {
			change, err = d.Store.DeleteEffectiveWindow(ctx, in.ID, rev)
		}
		if err != nil {
			return FromStoreError(err)
		}
		out = validityReceipt(change)
		return nil
	})
	return out, err
}
