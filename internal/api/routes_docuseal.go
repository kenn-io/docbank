package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/docusealclient"
)

func registerDocuSealRoutes(api huma.API, d Deps) {
	client := func(ctx context.Context) (*docusealclient.Client, error) {
		connection, err := d.Cfg.Integrations.DocuSeal.Resolve(ctx, d.VaultRoot)
		if err != nil {
			if d.Cfg.Integrations.DocuSeal.DocuSealDisabled() {
				return nil, NewError(503, "docuseal_unconfigured", "Enable and configure [integrations.docuseal]")
			}
			return nil, NewError(503, "docuseal_config_invalid", err.Error())
		}
		c, err := docusealclient.New(connection)
		if err != nil {
			return nil, NewError(503, "docuseal_config_invalid", "Check DocuSeal egress settings")
		}
		return c, nil
	}
	problem := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) {
			return NewError(504, "docuseal_timeout", "DocuSeal request timed out")
		}
		switch err.Error() {
		case "docuseal_unauthorized":
			return NewError(502, "docuseal_unauthorized", "Check the DocuSeal credential")
		case "docuseal_not_found":
			return NewError(502, "docuseal_not_found", "Check the DocuSeal API path")
		case "docuseal_response_too_large", "docuseal_invalid_response", "docuseal_response_failed", "docuseal_provider_error":
			return NewError(502, err.Error(), "Check the DocuSeal provider response")
		default:
			return NewError(502, "docuseal_request_failed", "Check the DocuSeal connection and egress settings")
		}
	}
	type statusOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         docusealclient.Status
	}
	huma.Register(api, huma.Operation{OperationID: "getDocuSealStatus", Method: http.MethodGet, Path: "/api/v1/integrations/docuseal/status", Summary: "Check the DocuSeal connection", Errors: []int{502, 503, 504}}, func(ctx context.Context, _ *struct{}) (*statusOutput, error) {
		c, err := client(ctx)
		if err != nil {
			return nil, err
		}
		result, err := c.Check(ctx)
		if err != nil {
			return nil, problem(err)
		}
		return &statusOutput{CacheControl: "no-store", Body: result}, nil
	})
	type templatesOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         docusealclient.TemplatePage
	}
	huma.Register(api, huma.Operation{RejectUnknownQueryParameters: true, OperationID: "listDocuSealTemplates", Method: http.MethodGet, Path: "/api/v1/integrations/docuseal/templates", Summary: "List DocuSeal templates", Errors: []int{400, 502, 503, 504}}, func(ctx context.Context, in *struct {
		After int64 `query:"after" minimum:"0"`
	}) (*templatesOutput, error) {
		c, err := client(ctx)
		if err != nil {
			return nil, err
		}
		result, err := c.Templates(ctx, in.After)
		if err != nil {
			return nil, problem(err)
		}
		return &templatesOutput{CacheControl: "no-store", Body: result}, nil
	})
}
