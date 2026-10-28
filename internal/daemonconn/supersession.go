package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// PutSupersession declares a replacement under the predecessor's revision.
func (c *Connection) PutSupersession(ctx context.Context, successorID, predecessorID, revision int64, note string) (api.ValidityReceipt, error) {
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).PutSupersession(ctx, &apiclient.PutSupersessionRequestOptions{PathParams: &apiclient.PutSupersessionPath{ID: successorID, PredecessorID: predecessorID}, Header: &apiclient.PutSupersessionHeaders{IfMatch: revisionIfMatch(revision)}, Body: &api.SupersessionRequest{Note: note}})
	return checkedValidityReceipt(receipt, response, predecessorID, revision, err)
}

// DeleteSupersession removes a replacement under the predecessor's revision.
func (c *Connection) DeleteSupersession(ctx context.Context, successorID, predecessorID, revision int64) (api.ValidityReceipt, error) {
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).DeleteSupersession(ctx, &apiclient.DeleteSupersessionRequestOptions{PathParams: &apiclient.DeleteSupersessionPath{ID: successorID, PredecessorID: predecessorID}, Header: &apiclient.DeleteSupersessionHeaders{IfMatch: revisionIfMatch(revision)}})
	return checkedValidityReceipt(receipt, response, predecessorID, revision, err)
}

// PutEffectiveWindow sets inclusive document dates under its node revision.
func (c *Connection) PutEffectiveWindow(ctx context.Context, nodeID, revision int64, request api.EffectiveWindowRequest) (api.ValidityReceipt, error) {
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).PutEffectiveWindow(ctx, &apiclient.PutEffectiveWindowRequestOptions{PathParams: &apiclient.PutEffectiveWindowPath{ID: nodeID}, Header: &apiclient.PutEffectiveWindowHeaders{IfMatch: revisionIfMatch(revision)}, Body: &request})
	return checkedValidityReceipt(receipt, response, nodeID, revision, err)
}

// DeleteEffectiveWindow clears document dates under its node revision.
func (c *Connection) DeleteEffectiveWindow(ctx context.Context, nodeID, revision int64) (api.ValidityReceipt, error) {
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).DeleteEffectiveWindow(ctx, &apiclient.DeleteEffectiveWindowRequestOptions{PathParams: &apiclient.DeleteEffectiveWindowPath{ID: nodeID}, Header: &apiclient.DeleteEffectiveWindowHeaders{IfMatch: revisionIfMatch(revision)}})
	return checkedValidityReceipt(receipt, response, nodeID, revision, err)
}

func checkedValidityReceipt(receipt *api.ValidityReceipt, response *http.Response, nodeID, revision int64, err error) (api.ValidityReceipt, error) {
	if err != nil {
		return api.ValidityReceipt{}, err
	}
	if receipt == nil || response == nil {
		return api.ValidityReceipt{}, &responseDecodeError{err: errors.New("document validity receipt is missing")}
	}
	wantRevision := revision
	if receipt.Changed {
		wantRevision++
	}
	if receipt.Node.ID != nodeID || receipt.Node.Revision != wantRevision || response.Header.Get("ETag") != revisionIfMatch(wantRevision) {
		return api.ValidityReceipt{}, &responseDecodeError{err: fmt.Errorf("document validity receipt does not bind node %d revision %d", nodeID, wantRevision)}
	}
	return *receipt, nil
}

// NodeValidity inspects declarations using the daemon's calendar date by default.
func (c *Connection) NodeValidity(ctx context.Context, nodeID int64, asOf string) (api.NodeValidity, error) {
	view, err := c.API().GetNodeValidity(ctx, &apiclient.GetNodeValidityRequestOptions{PathParams: &apiclient.GetNodeValidityPath{ID: nodeID}, Query: &apiclient.GetNodeValidityQuery{AsOf: &asOf}})
	if err != nil {
		return api.NodeValidity{}, err
	}
	if view == nil || view.Node.ID != nodeID || view.Node.Revision < 1 || view.AsOf == "" || document.ValidateDate(view.AsOf) != nil || (asOf != "" && view.AsOf != asOf) {
		return api.NodeValidity{}, &responseDecodeError{err: errors.New("document validity response does not bind the requested node and date")}
	}
	return *view, nil
}
