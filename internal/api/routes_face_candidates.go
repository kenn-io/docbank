package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type FaceCandidatePage struct {
	Items []store.FaceCandidate `json:"items"`
	Total int64                 `json:"total"`
}
type FaceCandidateDecisionRequest struct {
	FaceIDs     []string `json:"face_ids,omitzero" maxItems:"48"`
	Action      string   `json:"action" enum:"link,reject,new_person"`
	PersonID    string   `json:"person_id,omitzero" format:"uuid"`
	DisplayName string   `json:"display_name,omitzero" maxLength:"200"`
}
type FaceCandidateDecisionResult struct {
	CandidateID string `json:"candidate_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
}

func registerFaceCandidateRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listFaceCandidates", Method: http.MethodGet, Path: "/api/v1/people/face-candidates", Summary: "Review suggested face groups"}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"24" minimum:"1" maximum:"250"`
		Offset int `query:"offset" minimum:"0" maximum:"10000"`
	}) (*struct{ Body FaceCandidatePage }, error) {
		items, total, err := d.Store.FaceCandidates(ctx, d.Blobs, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body FaceCandidatePage }{FaceCandidatePage{Items: items, Total: total}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "decideFaceCandidate", Method: http.MethodPost, Path: "/api/v1/people/face-candidates/{candidate_id}/decision", Summary: "Confirm or dismiss a suggested face group"}, func(ctx context.Context, in *struct {
		CandidateID string `path:"candidate_id" format:"uuid"`
		IfMatch     string `header:"If-Match"`
		Body        FaceCandidateDecisionRequest
	}) (*struct {
		ETag string `header:"ETag"`
		Body FaceCandidateDecisionResult
	}, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result store.PersonMatchCandidate
		err = g.mutate(ctx, func() error {
			var err error
			result, err = d.Store.DecidePersonCandidate(browserPeopleScope(ctx), d.Blobs, store.CandidateDecision{CandidateID: in.CandidateID, Action: in.Body.Action, PersonID: in.Body.PersonID, DisplayName: in.Body.DisplayName, FaceIDs: in.Body.FaceIDs, ExpectedRevision: revision, FaceOnly: true})
			return FromStoreError(err)
		})
		if err != nil {
			return nil, err
		}
		return &struct {
			ETag string `header:"ETag"`
			Body FaceCandidateDecisionResult
		}{revisionETag(result.Revision), FaceCandidateDecisionResult{result.CandidateID, result.Revision, result.State}}, nil
	})
}
