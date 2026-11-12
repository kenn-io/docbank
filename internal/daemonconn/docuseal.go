package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/docusealclient"
)

func (c *Connection) DocuSealStatus(ctx context.Context) (docusealclient.Status, error) {
	response, err := c.API().GetDocuSealStatus(ctx)
	if err != nil {
		return docusealclient.Status{}, err
	}
	return *response, nil
}

func (c *Connection) DocuSealTemplates(ctx context.Context, after int64) (docusealclient.TemplatePage, error) {
	if after < 0 {
		return docusealclient.TemplatePage{}, errors.New("invalid cursor")
	}
	response, err := c.API().ListDocuSealTemplates(ctx, &apiclient.ListDocuSealTemplatesRequestOptions{Query: &apiclient.ListDocuSealTemplatesQuery{After: &after}})
	if err != nil {
		return docusealclient.TemplatePage{}, err
	}
	return *response, nil
}
