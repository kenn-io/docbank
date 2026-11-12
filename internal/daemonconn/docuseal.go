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
	if result.Templates == nil || len(result.Templates) > 100 || result.Next < 0 {
		return docusealclient.TemplatePage{}, &responseDecodeError{err: errors.New("invalid DocuSeal templates response")}
	}
	for _, item := range result.Templates {
		if item.ID <= 0 || item.Name == "" || len(item.Name) > 512 || len(item.Roles) > 20 {
			return docusealclient.TemplatePage{}, &responseDecodeError{err: errors.New("invalid DocuSeal template")}
		}
		for _, role := range item.Roles {
			if role == "" || len(role) > 512 {
				return docusealclient.TemplatePage{}, &responseDecodeError{err: errors.New("invalid DocuSeal role")}
			}
		}
	}
	return result.TemplatePage, nil
}
