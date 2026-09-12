package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"go.kenn.io/docbank/document"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func (c *Client) PublishEmailDocuments(ctx context.Context, r document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	digest, err := document.EmailDocumentRequestDigest(r)
	if err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	var out document.EmailDocumentPublicationReceipt
	if err = c.doEmailDocuments(ctx, http.MethodPost, "/api/v1/email-document-publications", r, &out); err != nil {
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
func (c *Client) EmailDocumentPublication(ctx context.Context, id string) (document.EmailDocumentPublicationReceipt, error) {
	var out document.EmailDocumentPublicationReceipt
	if err := document.ValidateEmailDocumentOperationID(id); err != nil {
		return out, err
	}
	err := c.doEmailDocuments(ctx, http.MethodGet, "/api/v1/email-document-publications/"+url.PathEscape(id), nil, &out)
	if err == nil {
		err = document.ValidateEmailDocumentReceipt(out)
		if out.OperationID != id {
			return document.EmailDocumentPublicationReceipt{}, integrityErrorf("wrong email receipt operation")
		}
	}
	return out, err
}
func (c *Client) EmailDocumentRelations(ctx context.Context, q document.EmailDocumentRelationQuery) (document.EmailDocumentRelationPage, error) {
	var out document.EmailDocumentRelationPage
	q, err := document.NormalizeEmailDocumentRelationQuery(q)
	if err != nil {
		return out, err
	}
	values := url.Values{"limit": {strconv.Itoa(q.Limit)}, "after_order": {strconv.Itoa(q.AfterOrder)}}
	if q.ParentVersionID != "" {
		values.Set("parent_version_id", q.ParentVersionID)
	} else {
		values.Set("child_version_id", q.ChildVersionID)
	}
	if q.AfterOperationID != "" {
		values.Set("after_operation_id", q.AfterOperationID)
	}
	err = c.doEmailDocuments(ctx, http.MethodGet, "/api/v1/email-document-relations?"+values.Encode(), nil, &out)
	if err == nil && len(out.Items) > q.Limit {
		return document.EmailDocumentRelationPage{}, integrityErrorf("email relation response exceeds requested page")
	}
	return out, err
}
func (c *Client) RemoveEmailDocumentPublication(ctx context.Context, id, digest string) error {
	if err := document.ValidateEmailDocumentOperationID(id); err != nil {
		return err
	}
	return c.doEmailDocuments(ctx, http.MethodDelete, "/api/v1/email-document-publications/"+url.PathEscape(id), struct {
		RequestDigest string `json:"request_digest"`
	}{digest}, nil)
}
func (c *Client) RequestEmailDocumentProcessing(ctx context.Context, r document.EmailDocumentProcessingRequest) (document.EmailDocumentProcessingReceipt, error) {
	var out document.EmailDocumentProcessingReceipt
	err := c.doEmailDocuments(ctx, http.MethodPost, "/api/v1/email-document-processing", r, &out)
	return out, err
}

func (c *Client) doEmailDocuments(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if len(data) > document.EmailDocumentMaxJSONBytes {
			return integrityErrorf("email document request exceeds byte limit")
		}
		body = bytes.NewReader(data)
	}
	req, err := c.emailRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, document.EmailDocumentMaxJSONBytes+1))
	if err != nil {
		return err
	}
	if len(data) > document.EmailDocumentMaxJSONBytes {
		return integrityErrorf("email document response exceeds byte limit")
	}
	if output == nil {
		return nil
	}
	return document.UnmarshalEmailDocumentJSON(data, output)
}
