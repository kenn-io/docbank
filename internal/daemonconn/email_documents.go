package daemonconn

import (
	"context"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"net/http"
)

func (c *Connection) PublishEmailDocuments(ctx context.Context, r document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	digest, err := document.EmailDocumentRequestDigest(r)
	if err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	var out document.EmailDocumentPublicationReceipt
	var responseHTTP *http.Response
	_, err = c.apiWithResponse(&responseHTTP).PublishEmailDocuments(runtime.WithStreamingResponse(ctx), &apiclient.PublishEmailDocumentsRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	if err != nil {
		return out, err
	}
	if out.OperationID != r.OperationID || out.RequestDigest != digest || len(out.Relations) > document.EmailDocumentMaxParts {
		return document.EmailDocumentPublicationReceipt{}, integrityErrorf("email publication receipt differs from request")
	}
	if err = document.ValidateEmailDocumentReceipt(out); err != nil {
		return document.EmailDocumentPublicationReceipt{}, integrityErrorf("invalid email receipt: %v", err)
	}
	return out, nil
}
func (c *Connection) EmailDocumentPublication(ctx context.Context, id string) (document.EmailDocumentPublicationReceipt, error) {
	var out document.EmailDocumentPublicationReceipt
	if err := document.ValidateEmailDocumentOperationID(id); err != nil {
		return out, err
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GetEmailDocumentPublication(runtime.WithStreamingResponse(ctx), &apiclient.GetEmailDocumentPublicationRequestOptions{PathParams: &apiclient.GetEmailDocumentPublicationPath{OperationID: id}})
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	if err == nil {
		err = document.ValidateEmailDocumentReceipt(out)
		if out.OperationID != id {
			return document.EmailDocumentPublicationReceipt{}, integrityErrorf("wrong email receipt operation")
		}
	}
	return out, err
}
func (c *Connection) EmailDocumentRelations(ctx context.Context, q document.EmailDocumentRelationQuery) (document.EmailDocumentRelationPage, error) {
	var out document.EmailDocumentRelationPage
	q, err := document.NormalizeEmailDocumentRelationQuery(q)
	if err != nil {
		return out, err
	}
	params := apiclient.ListEmailDocumentRelationsQuery{Limit: &q.Limit, AfterOrder: &q.AfterOrder}
	if q.ParentVersionID != "" {
		params.ParentVersionID = &q.ParentVersionID
	} else {
		params.ChildVersionID = &q.ChildVersionID
	}
	if q.AfterOperationID != "" {
		params.AfterOperationID = &q.AfterOperationID
	}
	var responseHTTP *http.Response
	_, err = c.apiWithResponse(&responseHTTP).ListEmailDocumentRelations(runtime.WithStreamingResponse(ctx), &apiclient.ListEmailDocumentRelationsRequestOptions{Query: &params})
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	if err == nil && len(out.Items) > q.Limit {
		return document.EmailDocumentRelationPage{}, integrityErrorf("email relation response exceeds requested page")
	}
	return out, err
}
func (c *Connection) RemoveEmailDocumentPublication(ctx context.Context, id, digest string) error {
	if err := document.ValidateEmailDocumentOperationID(id); err != nil {
		return err
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).RemoveEmailDocumentPublication(runtime.WithStreamingResponse(ctx), &apiclient.RemoveEmailDocumentPublicationRequestOptions{PathParams: &apiclient.RemoveEmailDocumentPublicationPath{OperationID: id}, Body: &apiclient.RemoveEmailDocumentPublicationBody{RequestDigest: digest}}, limitEmailDocumentRequest)
	if err != nil {
		return err
	}
	return decodeEmailDocuments(responseHTTP, nil)
}
func (c *Connection) RequestEmailDocumentProcessing(ctx context.Context, r document.EmailDocumentProcessingRequest) (document.EmailDocumentProcessingReceipt, error) {
	var out document.EmailDocumentProcessingReceipt
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).RequestEmailDocumentProcessing(runtime.WithStreamingResponse(ctx), &apiclient.RequestEmailDocumentProcessingRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	return out, err
}

// decodeEmailDocuments bounds JSON before applying the document contract decoder.
func decodeEmailDocuments(response *http.Response, output any) error {
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, document.EmailDocumentMaxJSONBytes+1))
	if err != nil {
		return &responseDecodeError{err: err}
	}
	if len(data) > document.EmailDocumentMaxJSONBytes {
		return &responseDecodeError{err: integrityErrorf("email document response exceeds byte limit")}
	}
	if output == nil {
		return nil
	}
	if err := document.UnmarshalEmailDocumentJSON(data, output); err != nil {
		return &responseDecodeError{err: err}
	}
	return nil
}

func limitEmailDocumentRequest(_ context.Context, request *http.Request) error {
	if request.ContentLength > document.EmailDocumentMaxJSONBytes {
		return integrityErrorf("email document request exceeds byte limit")
	}
	return nil
}
