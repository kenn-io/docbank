package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validateContractResponse(p *api.Contract, response *http.Response, id string) (api.Contract, error) {
	if p == nil || response == nil || !validUUIDv4(p.ContractID) || p.Revision < 1 || (id != "" && p.ContractID != id) || response.Header.Get("ETag") != revisionIfMatch(p.Revision) {
		return api.Contract{}, &responseDecodeError{err: errors.New("invalid contract response")}
	}
	return *p, nil
}
func (c *Connection) Contracts(ctx context.Context, status, kind, partyID, expiringBefore, q, asOf string, limit, offset int) (api.ContractPage, error) {
	lim, off := int64(limit), int64(offset)
	p, err := c.API().ListContracts(ctx, &apiclient.ListContractsRequestOptions{Query: &apiclient.ListContractsQuery{Status: &status, Type: &kind, PartyID: &partyID, ExpiringBefore: &expiringBefore, Q: &q, AsOf: &asOf, Limit: &lim, Offset: &off}})
	if err != nil {
		return api.ContractPage{}, err
	}
	if p == nil || p.Limit != limit || p.Offset != offset || len(p.Items) > limit {
		return api.ContractPage{}, &responseDecodeError{err: errors.New("invalid contract page")}
	}
	return *p, nil
}
func (c *Connection) Contract(ctx context.Context, id, asOf string) (api.Contract, error) {
	if !validUUIDv4(id) {
		return api.Contract{}, errors.New("invalid contract ID")
	}
	var r *http.Response
	p, err := c.apiWithResponse(&r).GetContract(ctx, &apiclient.GetContractRequestOptions{PathParams: &apiclient.GetContractPath{ContractID: id}, Query: &apiclient.GetContractQuery{AsOf: &asOf}})
	if err != nil {
		return api.Contract{}, err
	}
	return validateContractResponse(p, r, id)
}
func (c *Connection) CreateContract(ctx context.Context, in api.CreateContractRequest) (api.Contract, error) {
	var r *http.Response
	p, err := c.apiWithResponse(&r).CreateContract(ctx, &apiclient.CreateContractRequestOptions{Body: &in})
	if err != nil {
		return api.Contract{}, mutationRequestError(r, err)
	}
	return validateContractResponse(p, r, "")
}
func (c *Connection) UpdateContract(ctx context.Context, id string, rev int64, in api.PatchContractRequest) (api.Contract, error) {
	if !validUUIDv4(id) || rev < 1 {
		return api.Contract{}, errors.New("invalid contract ID or revision")
	}
	var r *http.Response
	p, err := c.apiWithResponse(&r).UpdateContract(ctx, &apiclient.UpdateContractRequestOptions{PathParams: &apiclient.UpdateContractPath{ContractID: id}, Header: &apiclient.UpdateContractHeaders{IfMatch: revisionIfMatch(rev)}, Body: &in})
	if err != nil {
		return api.Contract{}, mutationRequestError(r, err)
	}
	return validateContractResponse(p, r, id)
}
func (c *Connection) DeleteContract(ctx context.Context, id string, rev int64) error {
	if !validUUIDv4(id) || rev < 1 {
		return errors.New("invalid contract ID or revision")
	}
	var r *http.Response
	_, err := c.apiWithResponse(&r).DeleteContract(ctx, &apiclient.DeleteContractRequestOptions{PathParams: &apiclient.DeleteContractPath{ContractID: id}, Header: &apiclient.DeleteContractHeaders{IfMatch: revisionIfMatch(rev)}})
	return mutationRequestError(r, err)
}
func (c *Connection) AttachContractParty(ctx context.Context, id string, rev int64, partyID, role string) (api.Contract, error) {
	var r *http.Response
	p, err := c.apiWithResponse(&r).AttachContractParty(ctx, &apiclient.AttachContractPartyRequestOptions{PathParams: &apiclient.AttachContractPartyPath{ContractID: id, PartyID: partyID}, Header: &apiclient.AttachContractPartyHeaders{IfMatch: revisionIfMatch(rev)}, Query: &apiclient.AttachContractPartyQuery{Role: apiclient.AttachContractPartyQueryRole(role)}})
	if err != nil {
		return api.Contract{}, mutationRequestError(r, err)
	}
	return validateContractResponse(p, r, id)
}
func (c *Connection) DetachContractParty(ctx context.Context, id string, rev int64, partyID, role string) (api.Contract, error) {
	var r *http.Response
	p, err := c.apiWithResponse(&r).DetachContractParty(ctx, &apiclient.DetachContractPartyRequestOptions{PathParams: &apiclient.DetachContractPartyPath{ContractID: id, PartyID: partyID}, Header: &apiclient.DetachContractPartyHeaders{IfMatch: revisionIfMatch(rev)}, Query: &apiclient.DetachContractPartyQuery{Role: apiclient.DetachContractPartyQueryRole(role)}})
	if err != nil {
		return api.Contract{}, mutationRequestError(r, err)
	}
	return validateContractResponse(p, r, id)
}
