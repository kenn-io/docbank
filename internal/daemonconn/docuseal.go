package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/docusealclient"
)

func (c *Connection) DocuSealStatus(ctx context.Context) (docusealclient.Status, error) {
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetDocuSealStatus(runtime.WithStreamingResponse(ctx))
	if err != nil {
		return docusealclient.Status{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var result struct {
		docusealclient.Status
		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &result, 3<<20); err != nil {
		return docusealclient.Status{}, &responseDecodeError{err: err}
	}
	if !result.Configured || !result.Enabled || result.Endpoint == "" || result.Capability != "template_submission" {
		return docusealclient.Status{}, &responseDecodeError{err: errors.New("invalid DocuSeal status response")}
	}
	return result.Status, nil
}

func (c *Connection) DocuSealTemplates(ctx context.Context, after int64) (docusealclient.TemplatePage, error) {
	if after < 0 {
		return docusealclient.TemplatePage{}, errors.New("invalid cursor")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).ListDocuSealTemplates(runtime.WithStreamingResponse(ctx), &apiclient.ListDocuSealTemplatesRequestOptions{Query: &apiclient.ListDocuSealTemplatesQuery{After: &after}})
	if err != nil {
		return docusealclient.TemplatePage{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var result struct {
		docusealclient.TemplatePage
		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &result, 3<<20); err != nil {
		return docusealclient.TemplatePage{}, &responseDecodeError{err: err}
	}
	if !docusealclient.ValidTemplatePage(result.TemplatePage) {
		return docusealclient.TemplatePage{}, &responseDecodeError{err: errors.New("invalid DocuSeal templates response")}
	}
	return result.TemplatePage, nil
}
