package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePartyResponse(p *api.Party, response *http.Response, id string) (api.Party, error) {
	if p == nil || response == nil {
		return api.Party{}, &responseDecodeError{err: errors.New("party response is incomplete")}
	}
	if !validUUIDv4(p.PartyID) || p.Revision < 1 || (id != "" && p.PartyID != id) || response.Header.Get("ETag") != revisionIfMatch(p.Revision) {
		return api.Party{}, &responseDecodeError{err: errors.New("party response has inconsistent identity, revision, or ETag")}
	}
	return *p, nil
}

func (c *Connection) Parties(ctx context.Context, kind string, own *bool, includeRetired bool, limit, offset int) (api.PartyPage, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return api.PartyPage{}, errors.New("invalid party page")
	}
	lim, off := int64(limit), int64(offset)
	var ownValue *string
	if own != nil {
		value := "false"
		if *own {
			value = "true"
		}
		ownValue = &value
	}
	options := &apiclient.ListPartiesRequestOptions{Query: &apiclient.ListPartiesQuery{Kind: &kind, Own: ownValue, IncludeRetired: &includeRetired, Limit: &lim, Offset: &off}}
	page, err := c.API().ListParties(ctx, options)
	if err != nil {
		return api.PartyPage{}, err
	}
	if page == nil || page.Limit != limit || page.Offset != offset || len(page.Items) > limit {
		return api.PartyPage{}, &responseDecodeError{err: errors.New("invalid party page")}
	}
	return *page, nil
}
func (c *Connection) Party(ctx context.Context, id string) (api.Party, error) {
	if !validUUIDv4(id) {
		return api.Party{}, errors.New("party ID must be a canonical UUIDv4")
	}
	var response *http.Response
	p, err := c.apiWithResponse(&response).GetParty(ctx, &apiclient.GetPartyRequestOptions{PathParams: &apiclient.GetPartyPath{PartyID: id}})
	if err != nil {
		return api.Party{}, err
	}
	return validatePartyResponse(p, response, id)
}
func (c *Connection) CreateParty(ctx context.Context, input api.CreatePartyRequest) (api.Party, error) {
	var response *http.Response
	p, err := c.apiWithResponse(&response).CreateParty(ctx, &apiclient.CreatePartyRequestOptions{Body: &input})
	if err != nil {
		return api.Party{}, mutationRequestError(response, err)
	}
	return validatePartyResponse(p, response, "")
}
func (c *Connection) UpdateParty(ctx context.Context, id string, revision int64, input api.PatchPartyRequest) (api.Party, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.Party{}, fmt.Errorf("invalid party ID or revision")
	}
	var response *http.Response
	p, err := c.apiWithResponse(&response).UpdateParty(ctx, &apiclient.UpdatePartyRequestOptions{PathParams: &apiclient.UpdatePartyPath{PartyID: id}, Header: &apiclient.UpdatePartyHeaders{IfMatch: revisionIfMatch(revision)}, Body: &input})
	if err != nil {
		return api.Party{}, mutationRequestError(response, err)
	}
	return validatePartyResponse(p, response, id)
}
func (c *Connection) RetireParty(ctx context.Context, id string, revision int64) (api.Party, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.Party{}, fmt.Errorf("invalid party ID or revision")
	}
	var response *http.Response
	p, err := c.apiWithResponse(&response).RetireParty(ctx, &apiclient.RetirePartyRequestOptions{PathParams: &apiclient.RetirePartyPath{PartyID: id}, Header: &apiclient.RetirePartyHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.Party{}, mutationRequestError(response, err)
	}
	return validatePartyResponse(p, response, id)
}
